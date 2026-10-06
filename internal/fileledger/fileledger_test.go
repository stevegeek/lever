package fileledger

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)

func rec(t *testing.T, op, agent, login string, at time.Time) Record {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	return Record{V: 1, Op: op, ID: id, Agent: agent, Login: login, Name: "a.pdf",
		Rel: "workers/" + agent + "/.lever-files/in/k/a.pdf", SHA256: strings.Repeat("ab", 32), Size: 3, At: at}
}

func TestAddListFind(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "files-ledger")
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode().Perm())
	}
	a := rec(t, OpUpload, "w1", "c@x", t0)
	b := rec(t, OpShare, "w1", "c@x", t0.Add(time.Minute))
	for _, r := range []Record{a, b} {
		if err := l.Add(r, nil); err != nil {
			t.Fatal(err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, "w1.jsonl")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", fi.Mode().Perm())
	}
	got, err := l.List("w1")
	if err != nil || len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID || !got[0].At.Equal(t0) {
		t.Fatalf("list %+v %v", got, err)
	}
	if r, ok, err := l.Find("w1", b.ID); err != nil || !ok || r.Op != OpShare {
		t.Fatalf("find %+v %v %v", r, ok, err)
	}
	if _, ok, _ := l.Find("w2", b.ID); ok {
		t.Fatal("found in another agent's file")
	}
}

func TestAddAllowRefusalWritesNothing(t *testing.T) {
	l, _ := Open(filepath.Join(t.TempDir(), "fl"))
	no := errors.New("rate")
	if err := l.Add(rec(t, OpShare, "w1", "c@x", t0), func([]Record) error { return no }); !errors.Is(err, no) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := l.List("w1"); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestAddRefusesABadRecord(t *testing.T) {
	l, _ := Open(filepath.Join(t.TempDir(), "fl"))
	for name, mut := range map[string]func(*Record){
		"op":    func(r *Record) { r.Op = "x" },
		"id":    func(r *Record) { r.ID = "zz" },
		"agent": func(r *Record) { r.Agent = "../x" },
		"rel":   func(r *Record) { r.Rel = "../x" },
		"sha":   func(r *Record) { r.SHA256 = "x" },
		"name":  func(r *Record) { r.Name = "" },
		"login": func(r *Record) { r.Login = "" },
		"size":  func(r *Record) { r.Size = -1 },
	} {
		r := rec(t, OpUpload, "w1", "c@x", t0)
		mut(&r)
		if err := l.Add(r, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestListSkipsTornAndForeignLines(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fl")
	l, _ := Open(dir)
	good := rec(t, OpUpload, "w1", "c@x", t0)
	if err := l.Add(good, nil); err != nil {
		t.Fatal(err)
	}
	other := rec(t, OpUpload, "w2", "c@x", t0)
	f, _ := os.OpenFile(filepath.Join(dir, "w1.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"v":1,"op":"upload","id":"` + other.ID + `","agent":"w2"}` + "\n" + `{"torn`)
	f.Close()
	got, err := l.List("w1")
	if err != nil || len(got) != 1 || got[0].ID != good.ID {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestUnsafeFileIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fl")
	l, _ := Open(dir)
	_ = l.Add(rec(t, OpUpload, "w1", "c@x", t0), nil)
	_ = os.Chmod(filepath.Join(dir, "w1.jsonl"), 0o666)
	if _, err := l.List("w1"); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("err = %v", err)
	}
}

func TestTwoLedgersOnOneDirSerialise(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fl")
	a, _ := Open(dir)
	b, _ := Open(dir)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	limit := func(prior []Record) error {
		if len(prior) >= 5 {
			return errors.New("rate")
		}
		mu.Lock()
		allowed++
		mu.Unlock()
		return nil
	}
	recs := make([]Record, 20)
	for i := range recs {
		recs[i] = rec(t, OpShare, "w1", "c@x", t0) // built here: no t.Fatal in a goroutine
	}
	for i, r := range recs {
		l := a
		if i%2 == 1 {
			l = b
		}
		wg.Add(1)
		go func() { defer wg.Done(); _ = l.Add(r, limit) }()
	}
	wg.Wait()
	if got, _ := a.List("w1"); len(got) != 5 || allowed != 5 {
		t.Fatalf("records %d allowed %d, want 5", len(got), allowed)
	}
}

func TestRotationKeepsBothFilesReadable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fl")
	l, _ := Open(dir)
	l.cap = 200 // test seam: rotate after one line
	first := rec(t, OpUpload, "w1", "c@x", t0)
	_ = l.Add(first, nil)
	second := rec(t, OpUpload, "w1", "c@x", t0)
	_ = l.Add(second, nil)
	if _, err := os.Stat(filepath.Join(dir, "w1.jsonl.1")); err != nil {
		t.Fatal("no rotation")
	}
	if got, _ := l.List("w1"); len(got) != 2 || got[0].ID != first.ID {
		t.Fatalf("%+v", got)
	}
	if !IsAgentFile("w1.jsonl") || !IsAgentFile("w1.jsonl.1") || IsAgentFile(".lock") || IsAgentFile("../w1.jsonl") {
		t.Fatal("IsAgentFile")
	}
}

func TestOpenTightensAnExistingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fl")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode().Perm())
	}
}

// A valid record of another agent in this agent's file (copied there by
// hand, or a bug) is not this agent's.
func TestListSkipsAValidRecordOfAnotherAgent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fl")
	l, _ := Open(dir)
	if err := l.Add(rec(t, OpUpload, "w1", "c@x", t0), nil); err != nil {
		t.Fatal(err)
	}
	other := rec(t, OpUpload, "w2", "c@x", t0)
	line, _ := json.Marshal(other)
	f, _ := os.OpenFile(filepath.Join(dir, "w1.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.Write(append(line, '\n'))
	f.Close()
	got, err := l.List("w1")
	if err != nil || len(got) != 1 || got[0].Agent != "w1" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, ok, _ := l.Find("w1", other.ID); ok {
		t.Fatal("found another agent's id in w1's file")
	}
}
