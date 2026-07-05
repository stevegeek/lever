package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCSV(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "todos.csv")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const sample = `id,task,due,priority,done
1,Ship the demo,2026-07-06,high,false
2,Water the plants,2026-07-05,normal,true
3,Write the standup,2026-07-05,high,false
`

func TestListAll(t *testing.T) {
	s := OpenStore(writeCSV(t, sample))
	got, err := s.List(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 todos, got %d: %+v", len(got), got)
	}
	if got[0].Task != "Ship the demo" || got[0].Priority != "high" || got[0].Done {
		t.Fatalf("first row parsed wrong: %+v", got[0])
	}
	if !got[1].Done {
		t.Fatalf("row 2 should be done: %+v", got[1])
	}
}

func TestListPendingOnly(t *testing.T) {
	s := OpenStore(writeCSV(t, sample))
	got, err := s.List(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("pending-only want 2, got %d: %+v", len(got), got)
	}
	for _, td := range got {
		if td.Done {
			t.Fatalf("pending list contains a done item: %+v", td)
		}
	}
}

func TestListMissingFileErrors(t *testing.T) {
	s := OpenStore(filepath.Join(t.TempDir(), "nope.csv"))
	if _, err := s.List(false); err == nil {
		t.Fatal("expected an error for a missing CSV")
	}
}
