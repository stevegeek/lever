package scion

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

func TestHelpListsScope(t *testing.T) {
	help := "Available scopes:\n  agent:attach                Attach to agent sessions\n  agent:lifecycle             Start, stop, suspend, restart, and restore agents\n  agent:manage (alias)  expands to ...\n"
	if !helpListsScope(help, "agent:lifecycle") {
		t.Fatal("agent:lifecycle is listed")
	}
	old := "  agent:attach                Attach to agent sessions\n  project:read   Read projects\n"
	if helpListsScope(old, "agent:lifecycle") {
		t.Fatal("an older scion does not list agent:lifecycle")
	}
	if helpListsScope("  see agent:lifecycle docs\n", "agent:lifecycle") {
		t.Fatal("only a whole first field counts")
	}
}

// KnowsUATScope reads the scope list from exactly `scion hub token create
// --help` (it needs no hub, so it answers on `lever up` after a pin change).
func TestKnowsUATScopeArgv(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion hub token create --help", proc.Result{Stdout: "Scopes:\n  agent:attach   Attach\n  agent:lifecycle   Start, stop\n"})
	c := New(f, Options{})
	has, err := c.KnowsUATScope(context.Background(), "agent:lifecycle")
	if err != nil || !has {
		t.Fatalf("has=%v err=%v", has, err)
	}
	if len(f.Calls) != 1 || strings.Join(f.Calls[0].Args, " ") != "hub token create --help" {
		t.Fatalf("calls = %+v; want exactly `scion hub token create --help`", f.Calls)
	}
	if has, _ := c.KnowsUATScope(context.Background(), "agent:reincarnate"); has {
		t.Fatal("an unlisted scope is unknown")
	}
}
