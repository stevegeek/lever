package cli

import (
	"strings"
	"testing"
)

func TestBrokerCommandWired(t *testing.T) {
	root := newHostRootWith(defaultFactory)
	var found bool
	for _, c := range root.Commands() {
		if c.Name() == "broker" {
			found = true
			subs := map[string]bool{}
			for _, s := range c.Commands() {
				subs[s.Name()] = true
			}
			if !subs["serve"] || !subs["bump-epoch"] {
				t.Fatalf("broker subcommands = %v", subs)
			}
		}
	}
	if !found {
		t.Fatal("`lever broker` not wired into the host root")
	}
}

func TestRevokeCommandWired(t *testing.T) {
	root := newHostRootWith(defaultFactory)
	for _, c := range root.Commands() {
		if c.Name() == "revoke" {
			if !strings.Contains(c.Use, "revoke") {
				t.Fatalf("revoke use = %q", c.Use)
			}
			return
		}
	}
	t.Fatal("`lever revoke` not wired into the host root")
}
