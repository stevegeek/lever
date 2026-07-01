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

func TestSelectPlannedIsRejected(t *testing.T) {
	_, err := Select("linux-docker", exec.RealRunner{}, "lever-x")
	if err == nil {
		t.Fatal("Select(linux-docker) should error — it is planned, not implemented")
	}
	if !strings.Contains(err.Error(), "planned") || !strings.Contains(err.Error(), "orbstack") {
		t.Errorf("error %q should name the status and the selectable set", err)
	}
}

func TestSelectUnknownIsRejected(t *testing.T) {
	_, err := Select("nope", exec.RealRunner{}, "lever-x")
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("Select(nope) err = %v, want an 'unknown backend' error", err)
	}
}

// TestConstructorsMatchImplementedStatus keeps the registry and the guarantee
// matrix in lockstep: exactly the Implemented candidates have a constructor.
func TestConstructorsMatchImplementedStatus(t *testing.T) {
	for _, c := range backend.Candidates {
		_, hasCtor := constructors[c.Name]
		wantCtor := c.Status == backend.StatusImplemented
		if hasCtor != wantCtor {
			t.Errorf("backend %q: hasConstructor=%v but Status=%q (implemented⇔constructor)", c.Name, hasCtor, c.Status)
		}
	}
}
