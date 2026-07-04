package guest

import (
	"context"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/exec"
)

func TestParseScionStateMarkerAndEntries(t *testing.T) {
	out := "MARKER 1\nENTRY lever__c857bb16 /lever\nENTRY scratch__b3b56fb7 /lever/groves/scratch\n"
	st := parseScionState(out)
	if !st.MarkerPresent {
		t.Fatal("MARKER 1 must parse as present")
	}
	if len(st.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(st.Entries), st.Entries)
	}
	if st.Entries[0].Name != "lever__c857bb16" || st.Entries[0].WorkspacePath != "/lever" {
		t.Fatalf("entry 0 wrong: %+v", st.Entries[0])
	}
	if st.Entries[1].WorkspacePath != "/lever/groves/scratch" {
		t.Fatalf("entry 1 workspace wrong: %+v", st.Entries[1])
	}
}

func TestParseScionStateMarkerAbsent(t *testing.T) {
	// The bad-teardown signature: registered, but the in-tree marker is gone.
	st := parseScionState("MARKER 0\nENTRY lever__c857bb16 /lever\n")
	if st.MarkerPresent {
		t.Fatal("MARKER 0 must parse as absent")
	}
	if len(st.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(st.Entries))
	}
}

func TestParseScionStateNoEntries(t *testing.T) {
	st := parseScionState("MARKER 1\n")
	if !st.MarkerPresent || len(st.Entries) != 0 {
		t.Fatalf("marker present, no entries; got present=%v entries=%d", st.MarkerPresent, len(st.Entries))
	}
}

func TestParseScionStateIgnoresJunk(t *testing.T) {
	// Malformed/short lines are skipped, not fatal (fail-safe: "nothing stale").
	st := parseScionState("\nENTRY only-two-fields\ngarbage line here\nMARKER\nENTRY a /x\n")
	if len(st.Entries) != 1 || st.Entries[0].Name != "a" {
		t.Fatalf("only the well-formed ENTRY should survive; got %+v", st.Entries)
	}
	if st.MarkerPresent {
		t.Fatal("a bare MARKER line (no value) must not read as present")
	}
}

// TestRemoveScionProjectConfigsIssuesThroughUserPrefix proves the removal
// script is emitted through the machine-only UserPrefix (mirroring
// ReadScionProjectState's transport) and that the script's rm loop targets the
// given workspace path.
func TestRemoveScionProjectConfigsIssuesThroughUserPrefix(t *testing.T) {
	for _, shape := range prefixShapes("lever-x") {
		t.Run(shape.name, func(t *testing.T) {
			f := exec.NewFakeRunner()
			f.Script(strings.Join(shape.userPrefix, " "), exec.Result{})
			g := Guest{Host: f, UserPrefix: shape.userPrefix}

			if err := g.RemoveScionProjectConfigs(context.Background(), "/lever/groves/scratch"); err != nil {
				t.Fatalf("RemoveScionProjectConfigs: %v", err)
			}
			if len(f.Calls) != 1 {
				t.Fatalf("expected 1 call, got %d: %+v", len(f.Calls), f.Calls)
			}
			call := f.Calls[0]
			wantPrefix := append(append([]string{}, shape.userPrefix[1:]...), "bash", "-lc")
			if call.Name != shape.userPrefix[0] || !equalPrefix(call.Args, wantPrefix) {
				t.Fatalf("call = %+v, want name %q then prefix %v", call, shape.userPrefix[0], wantPrefix)
			}
			script := call.Args[len(call.Args)-1]
			if !strings.Contains(script, "'/lever/groves/scratch'") {
				t.Errorf("script missing quoted target workspace path: %q", script)
			}
			if !strings.Contains(script, "project-configs") || !strings.Contains(script, "workspace_path:") {
				t.Errorf("script missing project-configs glob or workspace_path grep: %q", script)
			}
			if !strings.Contains(script, "rm -rf") {
				t.Errorf("script missing the rm -rf removal: %q", script)
			}
		})
	}
}

// TestRemoveScionProjectConfigsErrorsOnGuestFailure proves it surfaces (not
// swallows) a failure of the guest command itself, mirroring
// ReadScionProjectState's error handling.
func TestRemoveScionProjectConfigsErrorsOnGuestFailure(t *testing.T) {
	f := exec.NewFakeRunner() // no Script registered ⇒ unscripted-command error
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "lever-x"}}

	if err := g.RemoveScionProjectConfigs(context.Background(), "/lever"); err == nil {
		t.Fatal("expected an error when the guest command fails, got nil")
	}
}
