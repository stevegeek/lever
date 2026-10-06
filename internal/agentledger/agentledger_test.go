package agentledger

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func auth(t *testing.T, agent, contact, kind, text string, at time.Time) Auth {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	return Auth{ID: id, Agent: agent, Contact: contact, Kind: kind, SHA256: HashText(text), Length: len([]rune(text)), Created: at, Expires: at.Add(TTL)}
}

func open(t *testing.T) (*Ledger, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "agent-ledger")
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return l, dir
}

func mustAuthorize(t *testing.T, l *Ledger, a Auth) {
	t.Helper()
	if err := l.Authorize(a, a.Created, func(View) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorizeWritesA0600LineWithoutText(t *testing.T) {
	l, dir := open(t)
	mustAuthorize(t, l, auth(t, "worker", "c@example.com", KindInitiated, "workbook v3 is ready", t0))
	files, _ := filepath.Glob(filepath.Join(dir, "c-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	fi, _ := os.Stat(files[0])
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(files[0])
	if strings.Contains(string(raw), "workbook") {
		t.Fatal("the record must hold the hash, never the text")
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v", di.Mode().Perm())
	}
}

func TestAuthorizeAllowSeesThePriorRecords(t *testing.T) {
	l, _ := open(t)
	mustAuthorize(t, l, auth(t, "worker", "c@x", KindInitiated, "one", t0))
	mustAuthorize(t, l, auth(t, "worker", "d@x", KindReply, "two", t0.Add(time.Minute)))
	var got View
	err := l.Authorize(auth(t, "worker", "c@x", KindInitiated, "three", t0.Add(2*time.Minute)), t0.Add(2*time.Minute),
		func(v View) error { got = v; return errors.New("no") })
	if err == nil || err.Error() != "no" {
		t.Fatalf("err = %v, want allow's error", err)
	}
	if len(got.ForContact) != 1 || got.ForContact[0].SHA256 != HashText("one") || got.LastHour != 2 {
		t.Fatalf("view = %+v", got)
	}
	v, _ := l.View("worker", "c@x", t0.Add(3*time.Minute))
	if len(v.ForContact) != 1 {
		t.Fatal("a refused authorization must not be written")
	}
}

func TestMatchOneRecordOneMessage(t *testing.T) {
	l, _ := open(t)
	a := auth(t, "worker", "c@x", KindInitiated, "hello", t0)
	mustAuthorize(t, l, a)
	msgs := []Candidate{
		{MessageID: "m2", SHA256: HashText("hello"), CreatedAt: t0.Add(2 * time.Minute)},
		{MessageID: "m1", SHA256: HashText("hello"), CreatedAt: t0.Add(time.Minute)},
	}
	keep, bound, err := l.Match("worker", "c@x", msgs, t0.Add(3*time.Minute))
	if err != nil || !keep["m1"] || keep["m2"] || len(bound) != 1 || bound[0] != a.ID {
		t.Fatalf("keep=%v bound=%v err=%v; want only the earliest", keep, bound, err)
	}
	// Later, newest page alone: m2 stays hidden, m1 stays shown.
	keep, _, _ = l.Match("worker", "c@x", msgs[:1], t0.Add(time.Hour))
	if keep["m2"] {
		t.Fatal("a bound record shows no second message")
	}
	keep, _, _ = l.Match("worker", "c@x", msgs[1:], t0.Add(48*time.Hour))
	if !keep["m1"] {
		t.Fatal("a bound message stays shown after the record expired")
	}
}

func TestMatchSurvivesReopen(t *testing.T) {
	l, dir := open(t)
	mustAuthorize(t, l, auth(t, "worker", "c@x", KindInitiated, "hello", t0))
	_, _, _ = l.Match("worker", "c@x", []Candidate{{MessageID: "m1", SHA256: HashText("hello"), CreatedAt: t0}}, t0)
	l2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	keep, _, _ := l2.Match("worker", "c@x", []Candidate{{MessageID: "m9", SHA256: HashText("hello"), CreatedAt: t0}}, t0)
	if keep["m9"] {
		t.Fatal("the binding must survive a restart")
	}
}

func TestMatchRules(t *testing.T) {
	for name, tc := range map[string]struct {
		agent, contact, text string
		at                   time.Duration // message time after the record
		want                 bool
	}{
		"exact":           {"worker", "c@x", "hello", time.Minute, true},
		"skew before":     {"worker", "c@x", "hello", -SkewBefore + time.Second, true},
		"sent before":     {"worker", "c@x", "hello", -SkewBefore - time.Second, false},
		"a minute before": {"worker", "c@x", "hello", -time.Minute, false},
		"skew after":      {"worker", "c@x", "hello", TTL + SkewAfter - time.Second, true},
		"after expiry":    {"worker", "c@x", "hello", TTL + SkewAfter + time.Second, false},
		"other text":      {"worker", "c@x", "hello!", time.Minute, false},
		"other agent":     {"other", "c@x", "hello", time.Minute, false},
		"other contact":   {"worker", "d@x", "hello", time.Minute, false},
	} {
		t.Run(name, func(t *testing.T) {
			l, _ := open(t)
			mustAuthorize(t, l, auth(t, "worker", "c@x", KindInitiated, "hello", t0))
			keep, _, err := l.Match(tc.agent, tc.contact, []Candidate{{MessageID: "m1", SHA256: HashText(tc.text), CreatedAt: t0.Add(tc.at)}}, t0.Add(time.Hour))
			if err != nil || keep["m1"] != tc.want {
				t.Fatalf("keep=%v err=%v want %v", keep, err, tc.want)
			}
		})
	}
}

func TestMatchBoundRowWithNewTextIsHidden(t *testing.T) {
	l, _ := open(t)
	mustAuthorize(t, l, auth(t, "worker", "c@x", KindInitiated, "hello", t0))
	_, _, _ = l.Match("worker", "c@x", []Candidate{{MessageID: "m1", SHA256: HashText("hello"), CreatedAt: t0}}, t0)
	for _, edited := range []string{"hello, edited", ""} {
		keep, _, _ := l.Match("worker", "c@x", []Candidate{{MessageID: "m1", SHA256: HashText(edited), CreatedAt: t0}}, t0)
		if keep["m1"] {
			t.Fatalf("an edited/deleted row (%q) must not show", edited)
		}
	}
}

func TestLedgerRefusesAnUnsafeDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent-ledger")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(dir, 0o777)
	if _, err := Open(dir); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("err = %v, want ErrUnsafe", err)
	}
}

func TestMatchIgnoresMalformedCandidates(t *testing.T) {
	l, _ := open(t)
	mustAuthorize(t, l, auth(t, "worker", "c@x", KindInitiated, "hello", t0))
	keep, _, err := l.Match("worker", "c@x", []Candidate{{MessageID: "bad id\n", SHA256: HashText("hello"), CreatedAt: t0}, {MessageID: "m1", SHA256: "XYZ", CreatedAt: t0}}, t0)
	if err != nil || len(keep) != 0 {
		t.Fatalf("keep=%v err=%v", keep, err)
	}
}

// The hourly count is per agent: another agent's authorizations never
// count, also when the count comes from the cache.
func TestLastHourIsPerAgent(t *testing.T) {
	l, dir := open(t)
	for i := range 30 {
		mustAuthorize(t, l, auth(t, "busy", "c@x", KindReply, "r", t0.Add(time.Duration(i)*time.Second)))
	}
	mustAuthorize(t, l, auth(t, "busy", "d@x", KindReply, "r", t0))
	now := t0.Add(time.Minute)
	for range 2 { // the second round reads the cache
		if v, _ := l.View("busy", "c@x", now); v.LastHour != 31 {
			t.Fatalf("busy = %d, want 31", v.LastHour)
		}
		if v, _ := l.View("quiet", "c@x", now); v.LastHour != 0 {
			t.Fatalf("quiet = %d, want 0", v.LastHour)
		}
	}
	if v, _ := l.View("busy", "c@x", t0.Add(2*time.Hour)); v.LastHour != 0 {
		t.Fatalf("an hour later = %d, want 0", v.LastHour)
	}
	// Another process (a new broker on the same directory) appends: the
	// cache sees it.
	l2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	mustAuthorize(t, l2, auth(t, "quiet", "c@x", KindInitiated, "x", t0))
	if v, _ := l.View("quiet", "c@x", now); v.LastHour != 1 {
		t.Fatalf("after another writer: %d, want 1", v.LastHour)
	}
}

// A second "shown" line for a message already bound (a racing or replaced
// broker) is ignored on read: the first binding decides what shows.
func TestReadKeepsTheFirstBindingOfAMessage(t *testing.T) {
	l, dir := open(t)
	a1 := auth(t, "worker", "c@x", KindInitiated, "hello", t0)
	a2 := auth(t, "worker", "c@x", KindInitiated, "other", t0)
	mustAuthorize(t, l, a1)
	mustAuthorize(t, l, a2)
	if keep, _, _ := l.Match("worker", "c@x", []Candidate{{MessageID: "m1", SHA256: HashText("hello"), CreatedAt: t0}}, t0); !keep["m1"] {
		t.Fatal("m1 binds to a1")
	}
	at := t0
	forged := line{V: 1, Op: "shown", ID: a2.ID, SHA256: HashText("other"), MessageID: "m1", At: &at}
	if err := (&hostledgerFile{dir: dir}).append(t, FileFor("c@x"), forged); err != nil {
		t.Fatal(err)
	}
	l2, _ := Open(dir)
	if keep, _, _ := l2.Match("worker", "c@x", []Candidate{{MessageID: "m1", SHA256: HashText("hello"), CreatedAt: t0}}, t0); !keep["m1"] {
		t.Fatal("the first binding of m1 must stand")
	}
	if keep, _, _ := l2.Match("worker", "c@x", []Candidate{{MessageID: "m1", SHA256: HashText("other"), CreatedAt: t0}}, t0); keep["m1"] {
		t.Fatal("a second binding of m1 must not show other text")
	}
}

// A binding that cannot be written is not shown, and is not kept: once the
// file is writable again the message binds normally.
func TestMatchKeepsNoBindingItCouldNotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only file")
	}
	l, dir := open(t)
	mustAuthorize(t, l, auth(t, "worker", "c@x", KindInitiated, "hello", t0))
	p := filepath.Join(dir, FileFor("c@x"))
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatal(err)
	}
	m := []Candidate{{MessageID: "m1", SHA256: HashText("hello"), CreatedAt: t0}}
	keep, bound, err := l.Match("worker", "c@x", m, t0)
	if err == nil || keep["m1"] || len(bound) != 0 {
		t.Fatalf("keep=%v bound=%v err=%v; want no binding", keep, bound, err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	keep, bound, err = l.Match("worker", "c@x", m, t0)
	if err != nil || !keep["m1"] || len(bound) != 1 {
		t.Fatalf("after the fix: keep=%v bound=%v err=%v", keep, bound, err)
	}
}

// Replies to one contact post are counted from the record, so a new ledger
// on the same directory sees them (the broker caps them at 3).
func TestReplyRefsSurviveReopen(t *testing.T) {
	l, dir := open(t)
	for range 3 {
		a := auth(t, "worker", "c@x", KindReply, "r", t0)
		a.ReplyTo = "post-1"
		mustAuthorize(t, l, a)
	}
	l2, _ := Open(dir)
	v, err := l2.View("worker", "c@x", t0)
	n := 0
	for _, a := range v.ForContact {
		if a.Kind == KindReply && a.ReplyTo == "post-1" {
			n++
		}
	}
	if err != nil || n != 3 {
		t.Fatalf("replies to post-1 after reopen = %d (%v)", n, err)
	}
}

type hostledgerFile struct{ dir string }

// append writes one raw line to a contact file, as another broker would.
func (h *hostledgerFile) append(t *testing.T, name string, v any) error {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(h.dir, name), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}
