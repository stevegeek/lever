package registry

import (
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/backend/orbstack"
	"github.com/lever-to/lever/internal/exec"
)

func TestSelectImplemented(t *testing.T) {
	b, err := Select("orbstack", exec.RealRunner{}, "lever-x")
	if err != nil {
		t.Fatalf("Select(orbstack): %v", err)
	}
	if _, ok := b.(*orbstack.OrbStack); !ok {
		t.Fatalf("Select(orbstack) = %T, want *orbstack.OrbStack", b)
	}
}

func TestSelectEmptyIsDefault(t *testing.T) {
	b, err := Select("", exec.RealRunner{}, "lever-x")
	if err != nil || b == nil {
		t.Fatalf("Select(\"\") = %v, %v; want the default backend", b, err)
	}
}

func TestSelectUnknownIsRejected(t *testing.T) {
	_, err := Select("nope", exec.RealRunner{}, "lever-x")
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("Select(nope) err = %v, want an 'unknown backend' error", err)
	}
}

// TestConstructorsMatchCandidates keeps the registry and the guarantee matrix
// in lockstep: exactly the declared candidates have a constructor.
func TestConstructorsMatchCandidates(t *testing.T) {
	if len(constructors) != len(backend.Candidates) {
		t.Fatalf("constructors has %d entries, Candidates has %d", len(constructors), len(backend.Candidates))
	}
	for _, c := range backend.Candidates {
		if _, ok := constructors[c.Name]; !ok {
			t.Errorf("candidate %q has no constructor", c.Name)
		}
	}
}
