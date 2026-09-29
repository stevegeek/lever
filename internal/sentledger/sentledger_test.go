package sentledger

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 29, 10, 0, 0, 400_000_000, time.UTC)

func open(t *testing.T) *Ledger {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "sent-ledger"), t0)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func send(t *testing.T, l *Ledger, cn, kind, body string, before time.Time, took time.Duration) Sent {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	e := Sent{ID: id, Recipient: cn, Slug: cn + "-slug", Kind: kind, Body: body, Before: before}
	if err := l.Begin(e); err != nil {
		t.Fatal(err)
	}
	if took >= 0 {
		if err := l.Done(e, before.Add(took), true); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// TestModes: the directory is 0700 and each file 0600.
func TestModes(t *testing.T) {
	l := open(t)
	e := send(t, l, "manager", KindOperatorNote, "hi", t0, time.Second)
	fi, err := os.Stat(l.Dir())
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, %v", fi.Mode().Perm(), err)
	}
	fi, err = os.Stat(filepath.Join(l.Dir(), FileFor(e.Recipient, e.Kind)))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, %v", fi.Mode().Perm(), err)
	}
}

// TestBeginDoneMergeAcrossRestart: a reopened ledger (a broker restart) holds
// the send with its end, and the exact body.
func TestBeginDoneMergeAcrossRestart(t *testing.T) {
	l := open(t)
	body := "[lever: operator note] ref=x\nline two\n\ttabs and ünïcode"
	e := send(t, l, "manager", KindOperatorNote, body, t0, 3*time.Second)
	l2, err := Open(l.Dir(), t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := l2.ByRef("manager", e.ID)
	if err != nil || !ok {
		t.Fatalf("ByRef = %v %v", ok, err)
	}
	if got.Body != body || got.Kind != KindOperatorNote || !got.Done() || !got.OK ||
		!got.Before.Equal(t0) || !got.After.Equal(t0.Add(3*time.Second)) || got.Slug != "manager-slug" {
		t.Fatalf("got %+v", got)
	}
}

// TestByRefIsBoundToTheRecipient: an id recorded for another recipient is a
// miss, so a ref one agent saw is worthless to another.
func TestByRefIsBoundToTheRecipient(t *testing.T) {
	l := open(t)
	e := send(t, l, "worker-a", KindManager, "for a", t0, time.Second)
	if _, ok, err := l.ByRef("manager", e.ID); ok || err != nil {
		t.Fatalf("another recipient's send matched: %v %v", ok, err)
	}
	if _, ok, _ := l.ByRef("worker-a", strings.Repeat("0", 32)); ok {
		t.Fatal("an unknown id matched")
	}
	if _, ok, _ := l.ByRef("worker-a", "not-an-id"); ok {
		t.Fatal("a malformed id matched")
	}
	if _, ok, _ := l.ByRef("worker-a", e.ID); !ok {
		t.Fatal("the recipient's own send did not match")
	}
}

// TestWindowEdges: the envelope second may sit from trunc(before)-5s to
// after+5s, and not a second beyond either edge.
func TestWindowEdges(t *testing.T) {
	l := open(t)
	e := send(t, l, "manager", KindWorkerPrefix+"alpha", "x", t0, 2*time.Second)
	lo := t0.Truncate(time.Second).Add(-ClockSkew)
	hi := t0.Add(2 * time.Second).Add(ClockSkew).Truncate(time.Second)
	now := t0.Add(time.Minute)
	for _, tc := range []struct {
		ts   time.Time
		want int
	}{
		{lo, 1}, {lo.Add(-time.Second), 0}, {hi, 1}, {hi.Add(time.Second), 0},
	} {
		got, err := l.InWindow("manager", tc.ts, now)
		if err != nil || len(got) != tc.want {
			t.Fatalf("ts %s: %d matches (%v), want %d", tc.ts, len(got), err, tc.want)
		}
		if tc.want == 1 && got[0].ID != e.ID {
			t.Fatalf("wrong match %+v", got[0])
		}
	}
}

// TestInFlightMatchesUntilMaxSendDuration: a send with no done line (a crash
// mid-call, or a verify during the call) matches up to before+2m.
func TestInFlightMatchesUntilMaxSendDuration(t *testing.T) {
	l := open(t)
	send(t, l, "manager", KindDirectiveNotice, "x", t0, -1)
	late := t0.Add(MaxSendDuration)
	if got, _ := l.InWindow("manager", late, t0.Add(time.Hour)); len(got) != 1 {
		t.Fatalf("in flight at +2m: %d matches", len(got))
	}
	if got, _ := l.InWindow("manager", late.Add(ClockSkew+time.Second), t0.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("in flight past +2m+skew: %d matches", len(got))
	}
	// Before the call could have finished, now bounds it.
	if got, _ := l.InWindow("manager", t0.Add(30*time.Second), t0.Add(10*time.Second)); len(got) != 0 {
		t.Fatal("an envelope from the future matched")
	}
}

// TestSameSecondSendsAreTwo: two identical bodies in one second are two
// entries, each with its own id.
func TestSameSecondSendsAreTwo(t *testing.T) {
	l := open(t)
	a := send(t, l, "manager", KindWorkerPrefix+"alpha", "same", t0, 0)
	b := send(t, l, "manager", KindWorkerPrefix+"alpha", "same", t0.Add(100*time.Millisecond), 0)
	got, err := l.InWindow("manager", t0.Truncate(time.Second), t0.Add(time.Minute))
	if err != nil || len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID {
		t.Fatalf("got %+v %v", got, err)
	}
}

// TestUnknownKindIsDropped: a line with a kind lever never writes is not a
// record, and Begin refuses to write one.
func TestUnknownKindIsDropped(t *testing.T) {
	l := open(t)
	id, _ := NewID()
	if err := l.Begin(Sent{ID: id, Recipient: "manager", Kind: "operator", Before: t0}); err == nil {
		t.Fatal("Begin wrote an unknown kind")
	}
	if err := l.Begin(Sent{ID: id, Recipient: "manager", Kind: "worker:", Before: t0}); err == nil {
		t.Fatal("Begin wrote an empty worker slug")
	}
	// Planted by hand in the manager's operator-note file.
	p := filepath.Join(l.Dir(), FileFor("manager", KindOperatorNote))
	line := `{"v":1,"op":"send","id":"` + id + `","recipient":"manager","kind":"owner","body":"x","before":"2026-09-29T10:00:00Z"}` + "\n"
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	l2, err := Open(l.Dir(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := l2.ByRef("manager", id); ok {
		t.Fatal("a line with an unknown kind was read")
	}
}

// TestFloodDoesNotRotateOutAnotherKind: a worker's flood of relays rotates
// only its own file; the operator note stays verifiable.
func TestFloodDoesNotRotateOutAnotherKind(t *testing.T) {
	l := open(t)
	note := send(t, l, "manager", KindOperatorNote, "keep me", t0, 0)
	big := strings.Repeat("x", 64<<10)
	for i := 0; i < 3*RotateCap/len(big); i++ {
		send(t, l, "manager", KindWorkerPrefix+"flood", big, t0, 0)
	}
	l2, err := Open(l.Dir(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := l2.ByRef("manager", note.ID); !ok || err != nil {
		t.Fatalf("the operator note was rotated out: %v %v", ok, err)
	}
	fi, err := os.Stat(filepath.Join(l.Dir(), FileFor("manager", KindWorkerPrefix+"flood")+".1"))
	if err != nil || fi.Size() > 2*RotateCap {
		t.Fatalf("the flooded file did not rotate: %v %v", fi, err)
	}
}

// TestRotationKeepsTheIndexInStep: after a rotation the index holds what the
// two files hold, no more (memory is bounded by the files).
func TestRotationKeepsTheIndexInStep(t *testing.T) {
	l := open(t)
	big := strings.Repeat("y", 256<<10)
	var ids []string
	for i := 0; i < 16; i++ {
		ids = append(ids, send(t, l, "manager", KindManager, big, t0, 0).ID)
	}
	if _, ok, _ := l.ByRef("manager", ids[0]); ok {
		t.Fatal("an entry rotated off disk is still indexed")
	}
	if _, ok, _ := l.ByRef("manager", ids[len(ids)-1]); !ok {
		t.Fatal("the newest entry is missing")
	}
	if len(l.index) > 12 {
		t.Fatalf("index holds %d entries, more than the files", len(l.index))
	}
}

// TestHandoffWritesAreSeen: a second process (a broker restart handoff)
// appending to the same directory is found on a lookup miss.
func TestHandoffWritesAreSeen(t *testing.T) {
	l := open(t)
	send(t, l, "manager", KindManager, "first", t0, 0)
	other, err := Open(l.Dir(), t0)
	if err != nil {
		t.Fatal(err)
	}
	e := send(t, other, "manager", KindManager, "from the other broker", t0, 0)
	got, ok, err := l.ByRef("manager", e.ID)
	if err != nil || !ok || got.Body != "from the other broker" {
		t.Fatalf("handoff write not seen: %+v %v %v", got, ok, err)
	}
	// The other process's lines in between our own appends are not lost.
	e2 := send(t, other, "manager", KindManager, "second", t0, 0)
	send(t, l, "manager", KindManager, "ours", t0, 0)
	if _, ok, _ := l.ByRef("manager", e2.ID); !ok {
		t.Fatal("a line the other broker wrote before our append was lost")
	}
}

// TestConcurrentWritersDuringRotation: two ledgers on one directory append
// concurrently across several rotations, and every entry still in the files
// is readable (no rotation drops a newer .1).
func TestConcurrentWritersDuringRotation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sent-ledger")
	a, err := Open(dir, t0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(dir, t0)
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("z", 100<<10)
	var mu sync.Mutex
	var ids []string
	var wg sync.WaitGroup
	for _, l := range []*Ledger{a, b} {
		wg.Add(1)
		go func(l *Ledger) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				id, _ := NewID()
				e := Sent{ID: id, Recipient: "manager", Kind: KindManager, Body: big, Before: t0}
				if err := l.Begin(e); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				ids = append(ids, id)
				mu.Unlock()
			}
		}(l)
	}
	wg.Wait()
	// The last ~RotateCap of appends must all be present: count the newest
	// ones found, in append order is not known, so check totals instead.
	c, err := Open(dir, t0)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, id := range ids {
		if _, ok, _ := c.ByRef("manager", id); ok {
			found++
		}
	}
	// Two files of at least RotateCap each hold at least 10 bodies of 100 KiB.
	if found < 10 {
		t.Fatalf("only %d of %d entries readable after concurrent rotation", found, len(ids))
	}
}

// TestUnsafeFilesAreRefused: a group-writable file, a symlinked file, and a
// group-writable directory are errors on lookup, never read.
func TestUnsafeFilesAreRefused(t *testing.T) {
	l := open(t)
	e := send(t, l, "manager", KindOperatorNote, "x", t0, 0)
	p := filepath.Join(l.Dir(), FileFor("manager", KindOperatorNote))
	if err := os.Chmod(p, 0o620); err != nil {
		t.Fatal(err)
	}
	l2, err := Open(l.Dir(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l2.ByRef("manager", e.ID); err == nil {
		t.Fatal("a group-writable file was read")
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.Dir(), 0o770); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l2.ByRef("manager", e.ID); err == nil {
		t.Fatal("a group-writable directory was used")
	}
	if err := l2.Begin(Sent{ID: e.ID, Recipient: "manager", Kind: KindManager, Before: t0}); err == nil {
		t.Fatal("Begin wrote into a group-writable directory")
	}
	_ = os.Chmod(l.Dir(), 0o700)

	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	q := filepath.Join(l.Dir(), FileFor("worker-a", KindManager))
	if err := os.Symlink(target, q); err != nil {
		t.Skip(err)
	}
	if _, _, err := l2.ByRef("worker-a", e.ID); err == nil {
		t.Fatal("a symlinked file was read")
	}
}

// TestOldFilesAreRemovedAtOpen: a file untouched for more than twice the
// window is deleted with its rotated copy; a recent one stays.
func TestOldFilesAreRemovedAtOpen(t *testing.T) {
	l := open(t)
	send(t, l, "worker-gone", KindManager, "old", t0, 0)
	keep := send(t, l, "manager", KindManager, "new", t0, 0)
	old := filepath.Join(l.Dir(), FileFor("worker-gone", KindManager))
	past := t0.Add(-3 * Window)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(l.Dir(), FileFor("manager", KindManager)), t0, t0); err != nil {
		t.Fatal(err)
	}
	l2, err := Open(l.Dir(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("the old file is still there: %v", err)
	}
	if _, ok, _ := l2.ByRef("manager", keep.ID); !ok {
		t.Fatal("a recent file was removed")
	}
}

// TestTornLineIsSkipped: a crash mid-write leaves a torn line; the entries
// around it still read.
func TestTornLineIsSkipped(t *testing.T) {
	l := open(t)
	a := send(t, l, "manager", KindManager, "a", t0, 0)
	p := filepath.Join(l.Dir(), FileFor("manager", KindManager))
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"v":1,"op":"send","id":"`)
	_ = f.Close()
	b := send(t, l, "manager", KindManager, "b", t0, 0)
	l2, err := Open(l.Dir(), t0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Sent{a, b} {
		if _, ok, err := l2.ByRef("manager", e.ID); !ok || err != nil {
			t.Fatalf("%s lost after a torn line: %v %v", e.Body, ok, err)
		}
	}
}

// TestValidKind: the four kinds, and nothing else.
func TestValidKind(t *testing.T) {
	for _, k := range []string{KindManager, KindOperatorNote, KindDirectiveNotice, "worker:alpha", "worker:a.b_c-1"} {
		if !ValidKind(k) {
			t.Errorf("%q refused", k)
		}
	}
	for _, k := range []string{"", "operator", "worker:", "worker:Alpha", "worker:a b", "worker:../x", "contact"} {
		if ValidKind(k) {
			t.Errorf("%q accepted", k)
		}
	}
}
