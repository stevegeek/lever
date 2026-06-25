package brokerctl

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/lever-to/lever/internal/config"
)

// Supervisor launches + tears down the configured first-party tool subprocesses.
// Tools are host-side, bind loopback, and self-register over the broker admin URL.
type Supervisor struct {
	tools    []config.Tool
	adminURL string
	logw     io.Writer

	mu   sync.Mutex
	cmds []*exec.Cmd
}

// NewSupervisor builds a supervisor for tools, injecting adminURL as each tool's
// -admin flag. logw receives each tool's combined stdout/stderr.
func NewSupervisor(tools []config.Tool, adminURL string, logw io.Writer) *Supervisor {
	return &Supervisor{tools: tools, adminURL: adminURL, logw: logw}
}

// Start launches every configured tool as a host subprocess: no shell, an
// explicit minimal env, and the configured command + injected -backend/-admin
// flags. It does not wait for registration (the caller health-checks the broker).
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tools {
		if len(t.Command) == 0 {
			return fmt.Errorf("brokerctl: tool %q has no command", t.Name)
		}
		args := append([]string{}, t.Command[1:]...)
		args = append(args, "-backend", t.Backend, "-admin", s.adminURL)
		cmd := exec.CommandContext(ctx, t.Command[0], args...)
		cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"} // minimal, no inherited secrets
		cmd.Stdout = s.logw
		cmd.Stderr = s.logw
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("brokerctl: start tool %q: %w", t.Name, err)
		}
		s.cmds = append(s.cmds, cmd)
	}
	return nil
}

// Stop signals every launched tool to terminate and reaps it.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cmd := range s.cmds {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}
	s.cmds = nil
}
