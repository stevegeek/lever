package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/scion"
)

type errInbox struct{}

func (errInbox) Inbox(context.Context, bool, string) ([]scion.Event, error) {
	return nil, errors.New("scion unreachable")
}

func TestPollOncePropagatesInboxErrorWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "events.log")
	b := New(errInbox{}, file)
	if _, err := b.PollOnce(context.Background()); err == nil {
		t.Fatal("PollOnce must propagate the Inbox error")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("PollOnce must not create the events file when Inbox fails")
	}
}

func TestPollOnceSkipsEventsWithoutID(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "events.log")
	// One event has no "id" (id guard at bridge.go:46) — it must be dropped, leaving
	// only the identified event written.
	fi := &fakeInbox{batches: [][]scion.Event{
		{{"type": "noise"}, {"id": "e1", "type": "input-needed"}},
	}}
	b := New(fi, file)
	fresh, err := b.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 || fresh[0].ID() != "e1" {
		t.Fatalf("want only e1, got %v", fresh)
	}
	data, _ := os.ReadFile(file)
	if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); len(lines) != 1 {
		t.Fatalf("want 1 written line, got %d: %q", len(lines), string(data))
	}
}

type fakeInbox struct {
	batches [][]scion.Event
	i       int
}

func (f *fakeInbox) Inbox(_ context.Context, _ bool, _ string) ([]scion.Event, error) {
	if f.i >= len(f.batches) {
		return nil, nil
	}
	b := f.batches[f.i]
	f.i++
	return b, nil
}

func TestPollOnceAppendsOnlyNewEvents(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "events.log")
	fi := &fakeInbox{batches: [][]scion.Event{
		{{"id": "e1", "type": "input-needed"}, {"id": "e2", "type": "state-change"}},
		{{"id": "e2", "type": "state-change"}, {"id": "e3", "type": "assistant-response"}},
	}}
	b := New(fi, file)

	n1, err := b.PollOnce(context.Background())
	if err != nil || len(n1) != 2 {
		t.Fatalf("poll1 n=%d err=%v", len(n1), err)
	}
	n2, err := b.PollOnce(context.Background())
	if err != nil || len(n2) != 1 || n2[0].ID() != "e3" {
		t.Fatalf("poll2 n=%d err=%v", len(n2), err)
	}
	data, _ := os.ReadFile(file)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines (e1,e2,e3), got %d: %q", len(lines), string(data))
	}
	if !strings.Contains(lines[0], `"e1"`) || !strings.Contains(lines[2], `"e3"`) {
		t.Fatalf("lines=%v", lines)
	}
}
