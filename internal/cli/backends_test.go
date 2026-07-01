package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/backend"
)

func TestBackendsCommandListsEveryCandidate(t *testing.T) {
	root := NewRootWithBackend(func(string) backend.Backend { return &stubBackend{} })
	root.SetArgs([]string{"backends"})
	var out bytes.Buffer
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatalf("backends: %v", err)
	}
	got := out.String()
	for _, name := range []string{"orbstack", "linux-docker", "lima", "apple-container"} {
		if !strings.Contains(got, name) {
			t.Errorf("output missing backend %q\n%s", name, got)
		}
	}
	for _, status := range []string{"implemented", "planned", "experimental"} {
		if !strings.Contains(got, status) {
			t.Errorf("output missing status %q\n%s", status, got)
		}
	}
}

// TestExactlyOneSelectableBackend is a tripwire: the CLI's defaultFactory ignores
// the configured backend name and builds the registry default, which is safe
// ONLY while orbstack is the sole selectable backend. Implementing a second one
// must be accompanied by threading app.Backend through the factory — at which
// point delete this test.
func TestExactlyOneSelectableBackend(t *testing.T) {
	if sel := backend.SelectableNames(); len(sel) != 1 {
		t.Fatalf("defaultFactory assumes a single selectable backend, got %v — make the factory name-aware before implementing a second backend", sel)
	}
}
