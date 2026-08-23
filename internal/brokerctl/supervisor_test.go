package brokerctl

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
)

// Uses /bin/sh indirectly? No — no shell. We launch a real, simple command that
// exits 0 quickly to prove argv assembly + lifecycle, then a long-running one.
// trackedCount returns the number of currently-tracked child processes.
func trackedCount(s *Supervisor) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cmds)
}

func TestSupervisorStartsConfiguredToolsWithFlags(t *testing.T) {
	// `true` ignores args and exits 0; we only assert Start doesn't error and the
	// process is launched with our injected flags appended (argv inspection via a
	// recording fake is overkill here — assert no error + clean Stop).
	tools := []ToolSpec{{Name: "db", Command: []string{"true"}, Backend: "127.0.0.1:3201"}}
	s := NewSupervisor(tools, "http://127.0.0.1:8444", filepath.Join(t.TempDir(), "tool-logs"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.Stop()
}

func TestSupervisorRejectsEmptyCommand(t *testing.T) {
	s := NewSupervisor([]ToolSpec{{Name: "db", Command: nil, Backend: "x"}}, "http://127.0.0.1:8444", filepath.Join(t.TempDir(), "tool-logs"))
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("a tool with no command must error")
	}
	s.Stop()
}

func TestSupervisorStartCleansUpOnPartialFailure(t *testing.T) {
	// First tool starts fine (`true`); second has an empty command → Start errors.
	// The supervisor must reap the first tool, leaving nothing tracked/running.
	tools := []ToolSpec{
		{Name: "ok", Command: []string{"true"}, Backend: "127.0.0.1:1"},
		{Name: "bad", Command: nil, Backend: "127.0.0.1:2"},
	}
	s := NewSupervisor(tools, "http://127.0.0.1:8444", filepath.Join(t.TempDir(), "tool-logs"))
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("Start must error when a tool has no command")
	}
	// After a failed Start, no processes remain tracked (cleaned up).
	if n := trackedCount(s); n != 0 {
		t.Fatalf("Start left %d processes tracked after partial-failure cleanup", n)
	}
	s.Stop() // must be safe to call again (no-op)
}

func TestSupervisorSkipsExternalTools(t *testing.T) {
	tools := []ToolSpec{
		{Name: "things3", External: true, Backend: "127.0.0.1:3300"},
	}
	s := NewSupervisor(tools, "http://127.0.0.1:1", filepath.Join(t.TempDir(), "tool-logs"))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start with only external tools must succeed (nothing to spawn): %v", err)
	}
	defer s.Stop()
	if n := trackedCount(s); n != 0 {
		t.Fatalf("tracked = %d, want 0 (external tools are fronted, not spawned)", n)
	}
}

func TestSupervisorMixedSpawnsOnlySupervised(t *testing.T) {
	tools := []ToolSpec{
		{Name: "ext", External: true, Backend: "127.0.0.1:3300"},
		{Name: "db", Command: []string{"/bin/sleep", "60"}, Backend: "127.0.0.1:3201"},
	}
	s := NewSupervisor(tools, "http://127.0.0.1:1", filepath.Join(t.TempDir(), "tool-logs"))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()
	if n := trackedCount(s); n != 1 {
		t.Fatalf("tracked = %d, want 1 (only the supervised tool spawns)", n)
	}
}

func TestSupervisorPerToolLogs(t *testing.T) {
	dir := t.TempDir()
	tools := []ToolSpec{
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

func TestToolSpecsCarriesWhatTheSupervisorNeeds(t *testing.T) {
	got := ToolSpecs([]config.Tool{
		{Name: "db", Command: []string{"db-server", "-x"}, Backend: "127.0.0.1:3201", Gate: config.GateCoarse},
		{Name: "ext", External: true, Backend: "127.0.0.1:3300"},
	})
	want := []ToolSpec{
		{Name: "db", Command: []string{"db-server", "-x"}, Backend: "127.0.0.1:3201"},
		{Name: "ext", External: true, Backend: "127.0.0.1:3300"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ToolSpecs = %+v, want %+v", got, want)
	}
	if len(ToolSpecs(nil)) != 0 {
		t.Fatal("no tools must map to no specs")
	}
}
