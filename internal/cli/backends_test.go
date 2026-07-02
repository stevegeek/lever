package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/backend"
)

func TestBackendsCommandListsEveryCandidate(t *testing.T) {
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return &stubBackend{}, nil })
	root.SetArgs([]string{"backends"})
	var out bytes.Buffer
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatalf("backends: %v", err)
	}
	got := out.String()
	for _, name := range []string{"orbstack", "lima"} {
		if !strings.Contains(got, name) {
			t.Errorf("output missing backend %q\n%s", name, got)
		}
	}
	for _, gone := range []string{"linux-docker", "apple-container", "planned", "experimental", "implemented"} {
		if strings.Contains(got, gone) {
			t.Errorf("output should no longer mention %q\n%s", gone, got)
		}
	}
}
