package brokerctl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/termsafe"
)

// ToolSpec is what the Supervisor needs to know about one configured tool:
// the name (for logs and errors), the command to spawn, the loopback backend
// it serves on, whether it is external (fronted by the broker, never
// spawned), and the directory it runs in. ToolSpecs builds them from config
// so the Supervisor itself holds no config types.
type ToolSpec struct {
	Name     string
	Command  []string
	Backend  string
	External bool
	// Dir is the tool's working directory and the base of a relative
	// command: the instance dir, where config load's tree check resolves
	// the same paths. Empty = the broker's own (tests).
	Dir string
	// ReadOnly lists the manager.read_only entries the tool's in-tree
	// program relies on (config.App.ToolsOnReadOnly). Such a tool starts
	// only once Supervisor.ReadOnlyGuard has seen the running manager
	// hold them read-only.
	ReadOnly []string
}

// ToolSpecs maps the configured broker tools onto the Supervisor's ToolSpec.
func ToolSpecs(app *config.App) ([]ToolSpec, error) {
	onRO, err := app.ToolsOnReadOnly()
	if err != nil {
		return nil, err
	}
	specs := make([]ToolSpec, 0, len(app.Broker.Tools))
	for _, t := range app.Broker.Tools {
		specs = append(specs, ToolSpec{Name: t.Name, Command: t.Command, Backend: t.Backend, External: t.External,
			Dir: app.InstanceDir(), ReadOnly: onRO[t.Name]})
	}
	return specs, nil
}

// ReadOnlyGuard reports, as nil, that the running manager holds every
// listed manager.read_only entry read-only; any other answer, including
// "cannot tell", is an error that holds the tool back.
type ReadOnlyGuard func(ctx context.Context, entries []string) error

// readOnlyRetry is how often the supervisor asks ReadOnlyGuard again for a
// tool it holds back: apply starts the broker before the manager, so a
// fresh create gains the mounts only after the first ask.
const readOnlyRetry = 30 * time.Second

// toolSecretEnv is the environment variable a supervised tool reads its
// broker-to-tool shared secret from (captool.ToolSecretEnv — spelled here so
// brokerctl does not import the tool SDK).
const toolSecretEnv = "LEVER_TOOL_SECRET"

// NewToolSecret mints the per-boot broker-to-tool shared secret: 32 random
// bytes, hex-encoded (header- and environment-safe). Serve mints one, hands it
// to the broker (Config.ToolSecret) and to every supervised tool (via the
// environment) — so a request that reaches a first-party tool's loopback port
// without passing through the broker is refused by the tool.
func NewToolSecret() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("brokerctl: tool secret: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// Supervisor launches + tears down the configured first-party tool subprocesses; external tools (broker-fronted, not spawned) are skipped.
// Tools are host-side, bind loopback, and self-register over the broker admin URL.
type Supervisor struct {
	tools      []ToolSpec
	adminURL   string
	toolLogDir string
	toolSecret string
	// ReadOnlyGuard vets each tool with ReadOnly entries before it starts
	// (nil = such a tool never starts: fail closed). retry is how often a
	// held-back tool is vetted again (readOnlyRetry when zero).
	ReadOnlyGuard ReadOnlyGuard
	retry         time.Duration

	mu      sync.Mutex
	cmds    []*exec.Cmd
	files   []*os.File
	stopped bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewSupervisor builds a supervisor for tools, injecting adminURL as each
// tool's -admin flag and toolSecret as its LEVER_TOOL_SECRET environment
// variable (NewToolSecret; the environment, never argv, so it is not
// ps-visible). Each supervised tool's combined stdout/stderr is written to its
// own <toolLogDir>/<name>.log so per-tool forensics aren't muddled in a shared
// file.
func NewSupervisor(tools []ToolSpec, adminURL, toolLogDir, toolSecret string) *Supervisor {
	return &Supervisor{tools: tools, adminURL: adminURL, toolLogDir: toolLogDir, toolSecret: toolSecret}
}

// Start launches every configured tool as a host subprocess: no shell, an
// explicit minimal env, and the configured command + injected -backend/-admin
// flags. It does not wait for registration (the caller health-checks the broker).
// If any tool fails to start, all already-started tools are force-killed and
// reaped before returning the error, leaving the supervisor clean.
//
// A tool whose program relies on manager.read_only (ToolSpec.ReadOnly) is
// held back until ReadOnlyGuard passes: a manager created before the
// entry, or one whose mounts cannot be read, could have rewritten the
// program. Its log says why; the guard is asked again every retry interval
// until it passes or Stop runs.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.toolLogDir, 0o700); err != nil {
		s.stopLocked()
		return fmt.Errorf("brokerctl: tool log dir: %w", err)
	}
	var held []ToolSpec
	for _, t := range s.tools {
		if t.External {
			continue // fronted, not spawned — lifecycle stays with the user session
		}
		if len(t.Command) == 0 {
			s.stopLocked()
			return fmt.Errorf("brokerctl: tool %q has no command", t.Name)
		}
		if len(t.ReadOnly) > 0 {
			held = append(held, t)
			continue
		}
		if err := s.spawnLocked(ctx, t); err != nil {
			s.stopLocked()
			return err
		}
	}
	if len(held) > 0 {
		loopCtx, cancel := context.WithCancel(ctx)
		s.cancel, s.done = cancel, make(chan struct{})
		go s.vetHeld(loopCtx, held)
	}
	return nil
}

// vetHeld asks ReadOnlyGuard for each held-back tool, starts those it
// passes, and asks again every retry interval for the rest.
func (s *Supervisor) vetHeld(ctx context.Context, held []ToolSpec) {
	defer close(s.done)
	retry := s.retry
	if retry <= 0 {
		retry = readOnlyRetry
	}
	reported := map[string]string{}
	for {
		var still []ToolSpec
		for _, t := range held {
			err := errors.New("no read-only check is wired into this broker")
			if s.ReadOnlyGuard != nil {
				err = s.ReadOnlyGuard(ctx, t.ReadOnly)
			}
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				// One line per reason class, not one per retry: the class
				// is fixed text (guardClass), so changing output cannot
				// defeat the dedupe; the message is sanitised and bounded.
				if class := guardClass(err); reported[t.Name] != class {
					reported[t.Name] = class
					s.logTool(t.Name, fmt.Sprintf("lever: not starting tool %q: its program lies in the tree under manager.read_only %v, "+
						"and %s. A manager without those read-only mounts could have rewritten it. To fix it, back up the manager's "+
						"conversation, then run `lever up --fresh` to recreate the manager with the mounts, and check the program "+
						"before you trust it again. Retrying every %s.",
						t.Name, t.ReadOnly, boundedReason(err), retry))
				}
				still = append(still, t)
				continue
			}
			s.mu.Lock()
			if !s.stopped {
				if err := s.spawnLocked(ctx, t); err != nil {
					s.logTool(t.Name, "lever: "+err.Error())
				}
			}
			s.mu.Unlock()
		}
		if held = still; len(held) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// maxReasonBytes bounds a guard refusal in a tool log line.
const maxReasonBytes = 512

// boundedReason is a guard refusal for a log line: terminal-safe and cut to
// maxReasonBytes, whatever produced it.
func boundedReason(err error) string {
	r := termsafe.Sanitize(err.Error())
	if len(r) > maxReasonBytes {
		r = strings.ToValidUTF8(r[:maxReasonBytes], "") + "…"
	}
	return r
}

// logTool appends one line to the tool's own log (best effort).
func (s *Supervisor) logTool(name, line string) {
	f, err := os.OpenFile(filepath.Join(s.toolLogDir, toolLogName(name)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

// spawnLocked starts one tool. Caller must hold s.mu.
func (s *Supervisor) spawnLocked(ctx context.Context, t ToolSpec) error {
	// Resolved here, not by exec: exec.Command would look a bare name
	// up on the broker's own PATH, not the fixed one config load
	// validated it against.
	bin, err := config.ResolveToolCommand(t.Command[0], t.Dir)
	if err != nil {
		return fmt.Errorf("brokerctl: tool %q: %w", t.Name, err)
	}
	args := append([]string{}, t.Command[1:]...)
	args = append(args, "-backend", t.Backend, "-admin", s.adminURL)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = t.Dir
	cmd.Env = []string{"PATH=" + config.ToolSupervisorPATH} // minimal, no inherited secrets
	if s.toolSecret != "" {
		cmd.Env = append(cmd.Env, toolSecretEnv+"="+s.toolSecret)
	}
	lf, err := os.OpenFile(filepath.Join(s.toolLogDir, toolLogName(t.Name)),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("brokerctl: open log for tool %q: %w", t.Name, err)
	}
	s.files = append(s.files, lf)
	cmd.Stdout = lf
	cmd.Stderr = lf
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("brokerctl: start tool %q: %w", t.Name, err)
	}
	s.cmds = append(s.cmds, cmd)
	return nil
}

// Stop force-kills (SIGKILL) every launched tool and reaps it.
// It is safe to call after a failed Start or multiple times.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	s.stopped = true
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

// stopLocked kills and reaps all tracked child processes and clears the slice.
// Caller must hold s.mu.
func (s *Supervisor) stopLocked() {
	for _, cmd := range s.cmds {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}
	s.cmds = nil
	for _, f := range s.files {
		_ = f.Close()
	}
	s.files = nil
}

// toolLogName maps a tool name to a safe per-tool log filename.
func toolLogName(name string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, name)
	return safe + ".log"
}
