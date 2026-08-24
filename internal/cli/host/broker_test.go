package host

import (
	"strings"
	"testing"
)

func TestBrokerCommandWired(t *testing.T) {
	wantSubcommands(t, "broker", "serve", "bump-epoch")
}

func TestRevokeCommandWired(t *testing.T) {
	root := newRootWith(defaultFactory)
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
