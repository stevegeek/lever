package registry

import (
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/exec"
)

func TestJailRunnerKnownAndUnknown(t *testing.T) {
	if jr, err := JailRunner("orbstack", exec.RealRunner{}, "lever-x", "u", "501"); err != nil || jr == nil {
		t.Fatalf("JailRunner(orbstack) = %v, %v", jr, err)
	}
	if jr, err := JailRunner("", exec.RealRunner{}, "lever-x", "u", "501"); err != nil || jr == nil {
		t.Fatalf("JailRunner(\"\") should use the default backend, got %v, %v", jr, err)
	}
	if _, err := JailRunner("nope", exec.RealRunner{}, "m", "u", "1"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("JailRunner(nope) err = %v, want unknown-backend error", err)
	}
}

func TestJailRunnerCoversAllCandidates(t *testing.T) {
	for _, c := range backend.Candidates {
		if _, err := JailRunner(c.Name, exec.RealRunner{}, "m", "u", "1"); err != nil {
			t.Errorf("JailRunner(%q): %v — every candidate must have a transport", c.Name, err)
		}
	}
}
