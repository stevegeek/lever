package main

import "testing"

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore("file:refdb?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReadReturnsFilteredRows(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.Read("A", "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r["owner"] != "alice" {
			t.Fatalf("filter leaked a non-alice row: %v", r)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("expected exactly 2 seeded alice rows in A, got %d", len(rows))
	}
}

func TestReadRejectsTableNotInAllowlist(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Read("C", "alice"); err == nil {
		t.Fatal("Read must reject table C (not allowlisted) as a defence-in-depth guard")
	}
}

func TestReadFilterIsBoundNotInterpolated(t *testing.T) {
	s := newTestStore(t)
	// An injection-shaped filter must simply match nothing, not error or leak.
	rows, err := s.Read("A", "alice' OR '1'='1")
	if err != nil {
		t.Fatalf("bound filter must be inert, got error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("injection-shaped filter must match no rows, got %d", len(rows))
	}
}
