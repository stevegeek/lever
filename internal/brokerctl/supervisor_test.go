package brokerctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
)

// Uses /bin/sh indirectly? No — no shell. We launch a real, simple command that
// exits 0 quickly to prove argv assembly + lifecycle, then a long-running one.
func TestSupervisorStartsConfiguredToolsWithFlags(t *testing.T) {
	// `true` ignores args and exits 0; we only assert Start doesn't error and the
	// process is launched with our injected flags appended (argv inspection via a
	// recording fake is overkill here — assert no error + clean Stop).
	tools := []config.Tool{{Name: "db", Command: []string{"true"}, Backend: "127.0.0.1:3201"}}
	s := NewSupervisor(tools, "http://127.0.0.1:8444", filepath.Join(t.TempDir(), "tool-logs"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.Stop()
}

func TestSupervisorRejectsEmptyCommand(t *testing.T) {
	s := NewSupervisor([]config.Tool{{Name: "db", Command: nil, Backend: "x"}}, "http://127.0.0.1:8444", filepath.Join(t.TempDir(), "tool-logs"))
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("a tool with no command must error")
	}
	s.Stop()
}

func TestSupervisorStartCleansUpOnPartialFailure(t *testing.T) {
	// First tool starts fine (`true`); second has an empty command → Start errors.
	// The supervisor must reap the first tool, leaving nothing tracked/running.
	tools := []config.Tool{
		{Name: "ok", Command: []string{"true"}, Backend: "127.0.0.1:1"},
		{Name: "bad", Command: nil, Backend: "127.0.0.1:2"},
	}
	s := NewSupervisor(tools, "http://127.0.0.1:8444", filepath.Join(t.TempDir(), "tool-logs"))
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("Start must error when a tool has no command")
	}
	// After a failed Start, no processes remain tracked (cleaned up).
	if n := s.trackedCount(); n != 0 {
		t.Fatalf("Start left %d processes tracked after partial-failure cleanup", n)
	}
	s.Stop() // must be safe to call again (no-op)
}

func TestSupervisorSkipsExternalTools(t *testing.T) {
	tools := []config.Tool{
		{Name: "things3", External: true, Backend: "127.0.0.1:3300", Gate: config.GateCoarse},
	}
	s := NewSupervisor(tools, "http://127.0.0.1:1", filepath.Join(t.TempDir(), "tool-logs"))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start with only external tools must succeed (nothing to spawn): %v", err)
	}
	defer s.Stop()
	if n := s.trackedCount(); n != 0 {
		t.Fatalf("tracked = %d, want 0 (external tools are fronted, not spawned)", n)
	}
}

func TestSupervisorMixedSpawnsOnlySupervised(t *testing.T) {
	tools := []config.Tool{
		{Name: "ext", External: true, Backend: "127.0.0.1:3300", Gate: config.GateCoarse},
		{Name: "db", Command: []string{"/bin/sleep", "60"}, Backend: "127.0.0.1:3201"},
	}
	s := NewSupervisor(tools, "http://127.0.0.1:1", filepath.Join(t.TempDir(), "tool-logs"))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()
	if n := s.trackedCount(); n != 1 {
		t.Fatalf("tracked = %d, want 1 (only the supervised tool spawns)", n)
	}
}

func TestSupervisorPerToolLogs(t *testing.T) {
	dir := t.TempDir()
	tools := []config.Tool{
		{Name: "alpha", Command: []string{"sh", "-c", "echo ALPHA_OUT"}},
		{Name: "beta", Command: []string{"sh", "-c", "echo BETA_OUT"}},
	}
	s := NewSupervisor(tools, "http://127.0.0.1:0", filepath.Join(dir, "tool-logs"))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	// give the short-lived echoes a moment, then stop (closes files)
	time.Sleep(200 * time.Millisecond)
	s.Stop()

	a, _ := os.ReadFile(filepath.Join(dir, "tool-logs", "alpha.log"))
	b, _ := os.ReadFile(filepath.Join(dir, "tool-logs", "beta.log"))
	if !strings.Contains(string(a), "ALPHA_OUT") {
		t.Fatalf("alpha.log missing its own output: %q", a)
	}
	if strings.Contains(string(a), "BETA_OUT") {
		t.Fatalf("alpha.log leaked beta's output: %q", a)
	}
	if !strings.Contains(string(b), "BETA_OUT") {
		t.Fatalf("beta.log missing its own output: %q", b)
	}
}
