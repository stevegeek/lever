package chatledger

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func entry(agent, sender, created, text, id string) Entry {
	return Entry{Recorded: time.Now().UTC(), Login: "op@example.com", Tier: TierOperator,
		Conversation: "dm:agent:" + agent + ":user:u1", AgentID: agent, MessageID: id,
		Sender: sender, CreatedAt: created, Text: text}
}

func TestAppendLookupMatchesAgentSenderAndSecond(t *testing.T) {
	p := filepath.Join(t.TempDir(), "chat-ledger.jsonl")
	w := (&fileWriter{path: p})
	for _, e := range []Entry{
		entry("a1", "user:op@example.com", "2026-09-28T10:00:00Z", "hello", "m1"),
		entry("a2", "user:op@example.com", "2026-09-28T10:00:00Z", "other agent", "m2"),
		entry("a1", "user:x@example.com", "2026-09-28T10:00:00Z", "other sender", "m3"),
		entry("a1", "user:op@example.com", "2026-09-28T10:00:01Z", "other second", "m4"),
	} {
		if err := w.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := lookupFile(p, "a1", "user:op@example.com", "2026-09-28T10:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "hello" || got[0].MessageID != "m1" {
		t.Fatalf("got %+v, want only m1", got)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("ledger mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestLookupMissingFileIsEmpty(t *testing.T) {
	got, err := Lookup(filepath.Join(t.TempDir(), "none.jsonl"), "a", "s", "t")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want no entries and no error", got, err)
	}
}

func TestLookupRefusesAWritableLedger(t *testing.T) {
	p := filepath.Join(t.TempDir(), "chat-ledger.jsonl")
	if err := (&fileWriter{path: p}).Append(entry("a1", "user:op", "2026-09-28T10:00:00Z", "hi", "m1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := lookupFile(p, "a1", "user:op", "2026-09-28T10:00:00Z"); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("err = %v, want ErrUnsafe", err)
	}
}

func TestAppendResetsModeOfAnExistingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "chat-ledger.jsonl")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := (&fileWriter{path: p}).Append(entry("a1", "user:op", "2026-09-28T10:00:00Z", "hi", "m1")); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestLookupReadsTheRotatedFileAndSkipsTornLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "chat-ledger.jsonl")
	w := (&fileWriter{path: p})
	if err := w.Append(entry("a1", "user:op", "2026-09-28T10:00:00Z", "old", "m1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p, p+".1"); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(entry("a1", "user:op", "2026-09-28T10:00:00Z", "new", "m2")); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"agent_id":"a1","sender":"user:op","created_at":"2026-09-28T10:0`)
	_ = f.Close()
	got, err := lookupFile(p, "a1", "user:op", "2026-09-28T10:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, e := range got {
		texts = append(texts, e.Text)
	}
	if strings.Join(texts, ",") != "old,new" {
		t.Fatalf("texts %v, want old,new", texts)
	}
}

func TestAppendRotatesPastTheCap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "chat-ledger.jsonl")
	if err := os.WriteFile(p, make([]byte, RotateCap+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&fileWriter{path: p}).Append(entry("a1", "user:op", "2026-09-28T10:00:00Z", "hi", "m1")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p + ".1"); err != nil || fi.Size() != RotateCap+1 {
		t.Fatalf("rotated file: %v, %v", fi, err)
	}
	if fi, _ := os.Stat(p); fi.Size() > 1024 {
		t.Fatalf("live file not restarted: %d bytes", fi.Size())
	}
}

func TestNormalizeTimestamp(t *testing.T) {
	for in, want := range map[string]string{
		"2026-09-28T10:00:00Z":           "2026-09-28T10:00:00Z",
		"2026-09-28T12:00:00+02:00":      "2026-09-28T10:00:00Z",
		"2026-09-28T10:00:00.987654321Z": "2026-09-28T10:00:00Z",
	} {
		got, err := NormalizeTimestamp(in)
		if err != nil || got != want {
			t.Errorf("NormalizeTimestamp(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeTimestamp("yesterday"); err == nil {
		t.Error("NormalizeTimestamp accepted a non-RFC 3339 value")
	}
}

func TestLookupRefusesASymlinkedLedger(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.jsonl")
	if err := (&fileWriter{path: target}).Append(entry("a1", "user:op", "2026-09-28T10:00:00Z", "hi", "m1")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "chat-ledger.jsonl")
	if err := os.Symlink(target, p); err != nil {
		t.Skip(err)
	}
	if _, err := lookupFile(p, "a1", "user:op", "2026-09-28T10:00:00Z"); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("err = %v, want ErrUnsafe for a symlink", err)
	}
}

func TestLookupSkipsAnOversizedLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "chat-ledger.jsonl")
	if err := os.WriteFile(p, append([]byte(strings.Repeat("x", maxLine+10)), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&fileWriter{path: p}).Append(entry("a1", "user:op", "2026-09-28T10:00:00Z", "after", "m1")); err != nil {
		t.Fatal(err)
	}
	got, err := lookupFile(p, "a1", "user:op", "2026-09-28T10:00:00Z")
	if err != nil || len(got) != 1 || got[0].Text != "after" {
		t.Fatalf("got %+v, %v; want the entry after the long line", got, err)
	}
}

func TestAppendDoesNotFollowASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "chat-ledger.jsonl")
	if err := os.Symlink(target, p); err != nil {
		t.Skip(err)
	}
	if err := (&fileWriter{path: p}).Append(entry("a1", "user:op", "2026-09-28T10:00:00Z", "hi", "m1")); err == nil {
		t.Fatal("Append wrote through a symlink")
	}
	if fi, _ := os.Stat(target); fi.Size() != 0 || fi.Mode().Perm() != 0o644 {
		t.Fatalf("symlink target changed: %v", fi)
	}
}

// TestDirKeepsLoginsApart: each login writes its own file, so one login's
// rotation never drops another's entries, and Lookup reads them all.
func TestDirKeepsLoginsApart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chat-ledger")
	w := NewWriter(dir)
	op := entry("a1", "user:op", "2026-09-28T10:00:00Z", "operator", "m1")
	c := entry("a1", "user:op", "2026-09-28T10:00:00Z", "contact", "m2")
	c.Login = "c@example.com"
	for _, e := range []Entry{op, c} {
		if err := w.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	// The contact floods its own file past two rotations.
	big := entry("a9", "user:c", "2026-09-28T11:00:00Z", strings.Repeat("<", 16000), "")
	big.Login = "c@example.com"
	for i := 0; i < 2*RotateCap/90000+4; i++ {
		if err := w.Append(big); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Lookup(dir, "a1", "user:op", "2026-09-28T10:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "operator" {
		t.Fatalf("got %+v, want the operator's entry to survive the contact's flood", got)
	}
	if fi, _ := os.Lstat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode().Perm())
	}
}

func TestLookupRefusesAnUnsafeDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chat-ledger")
	if err := NewWriter(dir).Append(entry("a1", "user:op", "2026-09-28T10:00:00Z", "hi", "m1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Lookup(dir, "a1", "user:op", "2026-09-28T10:00:00Z"); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("err = %v, want ErrUnsafe", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	_ = os.Chmod(dir, 0o700)
	if err := os.Symlink(dir, link); err != nil {
		t.Skip(err)
	}
	if _, err := Lookup(link, "a1", "user:op", "2026-09-28T10:00:00Z"); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("symlinked dir: err = %v, want ErrUnsafe", err)
	}
}
