package registry

import (
	"slices"
	"strings"
	"testing"

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

// TestEntriesAreComplete keeps every table entry whole and uniquely named: a
// named profile, a constructor and a jail prefix.
func TestEntriesAreComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range backends {
		if e.Profile.Name == "" || seen[e.Profile.Name] {
			t.Errorf("entry %+v has a missing or duplicate name", e.Profile)
		}
		seen[e.Profile.Name] = true
		if e.New == nil || e.JailPrefix == nil {
			t.Errorf("entry %q is incomplete", e.Profile.Name)
		}
	}
}

func TestNamesSortedAndCandidatesInTableOrder(t *testing.T) {
	if got, want := Names(), []string{"lima", "orbstack"}; !slices.Equal(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	var got []string
	for _, p := range Candidates() {
		got = append(got, p.Name)
	}
	if want := []string{"orbstack", "lima"}; !slices.Equal(got, want) {
		t.Fatalf("Candidates() names = %v, want %v", got, want)
	}
}

func TestProfileForKnownAndUnknown(t *testing.T) {
	if p, ok := ProfileFor("orbstack"); !ok || p != orbstack.Profile {
		t.Fatalf("ProfileFor(orbstack) = %+v, %v", p, ok)
	}
	if _, ok := ProfileFor("no-such-backend"); ok {
		t.Fatal("ProfileFor(unknown) should report ok=false")
	}
}
