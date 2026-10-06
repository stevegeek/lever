package agentledger

import (
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
		"exact":         {"worker", "c@x", "hello", time.Minute, true},
		"skew before":   {"worker", "c@x", "hello", -time.Minute, true},
		"too early":     {"worker", "c@x", "hello", -3 * time.Minute, false},
		"after expiry":  {"worker", "c@x", "hello", TTL + Skew + time.Second, false},
		"other text":    {"worker", "c@x", "hello!", time.Minute, false},
		"other agent":   {"other", "c@x", "hello", time.Minute, false},
		"other contact": {"worker", "d@x", "hello", time.Minute, false},
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
