package guest

import "testing"

func TestParseScionStateMarkerAndEntries(t *testing.T) {
	out := "MARKER 1\nENTRY lever__c857bb16 /lever\nENTRY scratch__b3b56fb7 /lever/groves/scratch\n"
	st := parseScionState(out)
	if !st.MarkerPresent {
		t.Fatal("MARKER 1 must parse as present")
	}
	if len(st.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(st.Entries), st.Entries)
	}
	if st.Entries[0].Name != "lever__c857bb16" || st.Entries[0].WorkspacePath != "/lever" {
		t.Fatalf("entry 0 wrong: %+v", st.Entries[0])
	}
	if st.Entries[1].WorkspacePath != "/lever/groves/scratch" {
		t.Fatalf("entry 1 workspace wrong: %+v", st.Entries[1])
	}
}

func TestParseScionStateMarkerAbsent(t *testing.T) {
	// The bad-teardown signature: registered, but the in-tree marker is gone.
	st := parseScionState("MARKER 0\nENTRY lever__c857bb16 /lever\n")
	if st.MarkerPresent {
		t.Fatal("MARKER 0 must parse as absent")
	}
	if len(st.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(st.Entries))
	}
}

func TestParseScionStateNoEntries(t *testing.T) {
	st := parseScionState("MARKER 1\n")
	if !st.MarkerPresent || len(st.Entries) != 0 {
		t.Fatalf("marker present, no entries; got present=%v entries=%d", st.MarkerPresent, len(st.Entries))
	}
}

func TestParseScionStateIgnoresJunk(t *testing.T) {
	// Malformed/short lines are skipped, not fatal (fail-safe: "nothing stale").
	st := parseScionState("\nENTRY only-two-fields\ngarbage line here\nMARKER\nENTRY a /x\n")
	if len(st.Entries) != 1 || st.Entries[0].Name != "a" {
		t.Fatalf("only the well-formed ENTRY should survive; got %+v", st.Entries)
	}
	if st.MarkerPresent {
		t.Fatal("a bare MARKER line (no value) must not read as present")
	}
}
