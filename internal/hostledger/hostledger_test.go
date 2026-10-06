package hostledger

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAppendAfterATornLineKeepsTheNewLine: a crash can leave a last line with
// no newline; the next append starts a new line, so only the torn one is
// lost.
func TestAppendAfterATornLineKeepsTheNewLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.jsonl")
	if err := os.WriteFile(p, []byte(`{"a":1}`+"\n"+`{"torn":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&File{Path: p, Label: "test", Cap: 1 << 20}).Append(map[string]int{"b": 2}); err != nil {
		t.Fatal(err)
	}
	var lines []string
	if err := ReadFile(p, "test", func(l []byte) { lines = append(lines, string(l)) }); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || lines[2] != `{"b":2}` {
		t.Fatalf("lines = %q", lines)
	}
}

// TestLockIsHeldAcrossTheAppend: File.Lock runs around every append.
func TestLockIsHeldAcrossTheAppend(t *testing.T) {
	dir := t.TempDir()
	held := 0
	f := &File{Path: filepath.Join(dir, "r.jsonl"), Label: "test", Cap: 1 << 20, Lock: func() (func(), error) {
		unlock, err := LockFile(filepath.Join(dir, ".lock"))
		held++
		return unlock, err
	}}
	if err := f.Append(1); err != nil || held != 1 {
		t.Fatalf("append: %v, lock taken %d times", err, held)
	}
}

// A rotate whose new file cannot be opened moves the full file back: no
// reader is left with a .1 and no main file.
func TestAppendRollsBackARotateItCannotFinish(t *testing.T) {
	dir := t.TempDir()
	w := &File{Path: filepath.Join(dir, "x.jsonl"), Label: "test", Cap: 10}
	if err := w.Append(map[string]string{"k": "a long enough first line"}); err != nil {
		t.Fatal(err)
	}
	orig := openFile
	openFile = func(string, int, os.FileMode) (*os.File, error) { return nil, os.ErrPermission }
	err := w.Append(map[string]string{"k": "second"})
	openFile = orig
	if err == nil {
		t.Fatal("the failed open must be an error")
	}
	if _, err := os.Stat(w.Path); err != nil {
		t.Fatalf("the main file must be back: %v", err)
	}
	if _, err := os.Stat(w.Path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("no .1 left behind: %v", err)
	}
	if err := w.Append(map[string]string{"k": "third"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Path + ".1"); err != nil {
		t.Fatalf("the next append rotates: %v", err)
	}
}
