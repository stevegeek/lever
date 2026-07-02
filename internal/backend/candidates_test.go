package backend

import "testing"

// Candidates lists exactly the implemented backends — roadmap and rejected
// backends are documentation (docs-site/_reference/backends.md), not code.
func TestCandidatesAreExactlyTheImplemented(t *testing.T) {
	want := []string{"lima", "orbstack"} // sorted
	got := Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
}

func TestCandidateProfileNameMatchesCandidateName(t *testing.T) {
	for _, c := range Candidates {
		if c.Profile.Name != c.Name {
			t.Errorf("candidate %q has Profile.Name %q", c.Name, c.Profile.Name)
		}
	}
}

func TestProfileForKnownAndUnknown(t *testing.T) {
	if p, ok := ProfileFor("orbstack"); !ok || p.Name != "orbstack" {
		t.Fatalf("ProfileFor(orbstack) = %+v, %v", p, ok)
	}
	if _, ok := ProfileFor("no-such-backend"); ok {
		t.Fatal("ProfileFor(unknown) should report ok=false")
	}
}

func TestLookupUnknown(t *testing.T) {
	if _, ok := Lookup("nope"); ok {
		t.Fatal("Lookup(nope) should be ok=false")
	}
}
