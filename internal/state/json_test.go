package state

import (
	"os"
	"path/filepath"
	"testing"
)

type jsonFixture struct {
	Epoch int
	Names []string
}

func TestLoadJSONAbsentIsZero(t *testing.T) {
	v, err := LoadJSON[jsonFixture](filepath.Join(t.TempDir(), "missing.json"), "fixture")
	if err != nil || v.Epoch != 0 || len(v.Names) != 0 {
		t.Fatalf("absent file: %+v %v", v, err)
	}
}

func TestLoadJSONGarbageErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJSON[jsonFixture](p, "fixture"); err == nil {
		t.Fatal("want parse error, got nil")
	}
}

// TestSaveJSONIsAtomicAndReplaces proves SaveJSON writes via a temp-file-then-
// rename (not a plain in-place write, which would torn-write on a mid-write
// crash): saving a second, different value over a first must fully replace
// it on reload, the file is 0600, and no .tmp-* scratch file survives.
func TestSaveJSONIsAtomicAndReplaces(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "v.json")
	if err := SaveJSON(p, "fixture", jsonFixture{Epoch: 1, Names: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveJSON(p, "fixture", jsonFixture{Epoch: 2, Names: []string{"b"}}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadJSON[jsonFixture](p, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if got.Epoch != 2 || len(got.Names) != 1 || got.Names[0] != "b" {
		t.Fatalf("second save did not fully replace the first: %+v", got)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if matched, _ := filepath.Match(".tmp-*", e.Name()); matched {
			t.Fatalf("leftover temp file after successful save: %s", e.Name())
		}
	}
}
