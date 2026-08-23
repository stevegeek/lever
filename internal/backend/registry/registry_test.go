package registry

import (
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/orbstack"
	"github.com/stevegeek/lever/internal/proc"
)

func TestSelectImplemented(t *testing.T) {
	b, err := Select("orbstack", proc.RealRunner{}, "lever-x")
	if err != nil {
		t.Fatalf("Select(orbstack): %v", err)
	}
	if _, ok := b.(*orbstack.OrbStack); !ok {
		t.Fatalf("Select(orbstack) = %T, want *orbstack.OrbStack", b)
	}
}

func TestSelectEmptyIsDefault(t *testing.T) {
	b, err := Select("", proc.RealRunner{}, "lever-x")
	if err != nil || b == nil {
		t.Fatalf("Select(\"\") = %v, %v; want the default backend", b, err)
	}
}

func TestSelectUnknownIsRejected(t *testing.T) {
	_, err := Select("nope", proc.RealRunner{}, "lever-x")
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("Select(nope) err = %v, want an 'unknown backend' error", err)
	}
}

// TestBackendsMatchCandidates keeps the registry and the guarantee matrix in
// lockstep: exactly the declared candidates have an entry, and every entry is
// complete. The two tables are separate only because backend (a leaf the
// config layer imports) cannot import the implementations.
func TestBackendsMatchCandidates(t *testing.T) {
	if len(backends) != len(backend.Candidates) {
		t.Fatalf("backends has %d entries, Candidates has %d", len(backends), len(backend.Candidates))
	}
	for _, c := range backend.Candidates {
		e, ok := backends[c.Name]
		if !ok {
			t.Errorf("candidate %q has no registry entry", c.Name)
			continue
		}
		if e.New == nil || e.JailPrefix == nil {
			t.Errorf("candidate %q has an incomplete registry entry", c.Name)
		}
	}
}
