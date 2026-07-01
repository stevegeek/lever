package backend

import "testing"

func TestCandidatesCoverAllKnownBackends(t *testing.T) {
	want := map[string]Status{
		"orbstack":        StatusImplemented,
		"linux-docker":    StatusPlanned,
		"lima":            StatusPlanned,
		"apple-container": StatusExperimental,
	}
	got := map[string]Status{}
	for _, c := range Candidates {
		got[c.Name] = c.Status
	}
	if len(got) != len(want) {
		t.Fatalf("Candidates has %d entries, want %d: %v", len(got), len(want), got)
	}
	for name, status := range want {
		if got[name] != status {
			t.Errorf("candidate %q status = %q, want %q", name, got[name], status)
		}
	}
}

func TestCandidateProfileNameMatchesCandidateName(t *testing.T) {
	// A candidate's declared Profile.Name must equal its Name, so ProfileFor and
	// the guarantee matrix cannot disagree about which backend a row describes.
	for _, c := range Candidates {
		if c.Profile.Name != c.Name {
			t.Errorf("candidate %q has Profile.Name %q", c.Name, c.Profile.Name)
		}
	}
}

func TestSelectableNamesAreExactlyTheImplemented(t *testing.T) {
	sel := SelectableNames()
	if len(sel) != 1 || sel[0] != "orbstack" {
		t.Fatalf("SelectableNames() = %v, want [orbstack]", sel)
	}
	for _, c := range Candidates {
		if IsSelectable(c.Name) != (c.Status == StatusImplemented) {
			t.Errorf("IsSelectable(%q)=%v but status=%q", c.Name, IsSelectable(c.Name), c.Status)
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
