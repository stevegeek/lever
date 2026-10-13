// Package bridge is the poll-based notification bridge: it appends each new
// Scion agent event (by id) as one JSON line to an events file, which the Lever
// manager watches with a Monitor. The file is the fixed interface; polling is
// an internal detail.
package bridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/stevegeek/lever/internal/retry"
	"github.com/stevegeek/lever/internal/scion"
)

// Inboxer is the slice of an inbox source the bridge needs.
type Inboxer interface {
	Inbox(ctx context.Context, unread bool, project string) ([]scion.Event, error)
}

type Bridge struct {
	in   Inboxer
	file string
	seen map[string]bool
}

func New(in Inboxer, eventsFile string) *Bridge {
	return &Bridge{in: in, file: eventsFile, seen: map[string]bool{}}
}

// PollOnce appends every event with an unseen id to the events file and
// returns the new events.
func (b *Bridge) PollOnce(ctx context.Context) ([]scion.Event, error) {
	events, err := b.in.Inbox(ctx, false, "")
	if err != nil {
		return nil, err
	}
	var fresh []scion.Event
	for _, e := range events {
		if id := e.ID(); id != "" && !b.seen[id] {
			fresh = append(fresh, e)
		}
	}
	if len(fresh) == 0 {
		return nil, nil
	}
	// Owner-only: the events carry message content.
	if err := os.MkdirAll(filepath.Dir(b.file), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(b.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	for _, e := range fresh {
		// Each line's message is marked as the worker's own text (idempotent over
		// what the broker already marked).
		line, err := json.Marshal(scion.WorkerReported(e))
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return nil, err
		}
		b.seen[e.ID()] = true
	}
	return fresh, nil
}

// Run polls forever at the given interval.
func (b *Bridge) Run(ctx context.Context, interval time.Duration) error {
	return retry.Until(ctx, 0, interval, func() (bool, error) {
		_, err := b.PollOnce(ctx)
		return false, err
	})
}
