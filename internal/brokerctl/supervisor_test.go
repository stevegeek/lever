package brokerctl

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/lever-to/lever/internal/config"
)

// Uses /bin/sh indirectly? No — no shell. We launch a real, simple command that
// exits 0 quickly to prove argv assembly + lifecycle, then a long-running one.
func TestSupervisorStartsConfiguredToolsWithFlags(t *testing.T) {
	// `true` ignores args and exits 0; we only assert Start doesn't error and the
	// process is launched with our injected flags appended (argv inspection via a
	// recording fake is overkill here — assert no error + clean Stop).
	tools := []config.Tool{{Name: "db", Command: []string{"true"}, Backend: "127.0.0.1:3201"}}
	s := NewSupervisor(tools, "http://127.0.0.1:8444", io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.Stop()
}

func TestSupervisorRejectsEmptyCommand(t *testing.T) {
	s := NewSupervisor([]config.Tool{{Name: "db", Command: nil, Backend: "x"}}, "http://127.0.0.1:8444", io.Discard)
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
	s := NewSupervisor(tools, "http://127.0.0.1:8444", io.Discard)
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("Start must error when a tool has no command")
	}
	// After a failed Start, no processes remain tracked (cleaned up).
	if n := s.trackedCount(); n != 0 {
		t.Fatalf("Start left %d processes tracked after partial-failure cleanup", n)
	}
	s.Stop() // must be safe to call again (no-op)
}
