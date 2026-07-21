package brokerctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/stevegeek/lever/internal/config"
)

// Supervisor launches + tears down the configured first-party tool subprocesses; external tools (broker-fronted, not spawned) are skipped.
// Tools are host-side, bind loopback, and self-register over the broker admin URL.
type Supervisor struct {
	tools      []config.Tool
	adminURL   string
	toolLogDir string

	mu    sync.Mutex
	cmds  []*exec.Cmd
	files []*os.File
}

// NewSupervisor builds a supervisor for tools, injecting adminURL as each
// tool's -admin flag. Each supervised tool's combined stdout/stderr is written
// to its own <toolLogDir>/<name>.log so per-tool forensics aren't muddled in a
// shared file.
func NewSupervisor(tools []config.Tool, adminURL, toolLogDir string) *Supervisor {
	return &Supervisor{tools: tools, adminURL: adminURL, toolLogDir: toolLogDir}
}

// Start launches every configured tool as a host subprocess: no shell, an
// explicit minimal env, and the configured command + injected -backend/-admin
// flags. It does not wait for registration (the caller health-checks the broker).
// If any tool fails to start, all already-started tools are force-killed and
// reaped before returning the error, leaving the supervisor clean.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.toolLogDir, 0o700); err != nil {
		s.stopLocked()
		return fmt.Errorf("brokerctl: tool log dir: %w", err)
	}
	for _, t := range s.tools {
		if t.External {
			continue // fronted, not spawned — lifecycle stays with the user session
		}
		if len(t.Command) == 0 {
			s.stopLocked()
			return fmt.Errorf("brokerctl: tool %q has no command", t.Name)
		}
		args := append([]string{}, t.Command[1:]...)
		args = append(args, "-backend", t.Backend, "-admin", s.adminURL)
		cmd := exec.CommandContext(ctx, t.Command[0], args...)
		cmd.Env = []string{"PATH=" + config.ToolSupervisorPATH} // minimal, no inherited secrets
		lf, err := os.OpenFile(filepath.Join(s.toolLogDir, toolLogName(t.Name)),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			s.stopLocked()
			return fmt.Errorf("brokerctl: open log for tool %q: %w", t.Name, err)
		}
		s.files = append(s.files, lf)
		cmd.Stdout = lf
		cmd.Stderr = lf
		if err := cmd.Start(); err != nil {
			s.stopLocked()
			return fmt.Errorf("brokerctl: start tool %q: %w", t.Name, err)
		}
		s.cmds = append(s.cmds, cmd)
	}
	return nil
}

// Stop force-kills (SIGKILL) every launched tool and reaps it.
// It is safe to call after a failed Start or multiple times.
func (s *Supervisor) Stop() {
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

// trackedCount returns the number of currently-tracked child processes (test aid).
func (s *Supervisor) trackedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cmds)
}
