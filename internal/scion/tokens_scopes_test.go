package scion

import "testing"

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
