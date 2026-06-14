// Package bridge is the poll-based notification bridge: it consumes Scion agent
// events via an Inboxer and appends each NEW event (by id) as one JSON line to
// an events file. The Lever manager arms a Monitor on that file, so it learns of
// agent events without polling itself. The watched file is the fixed interface;
// poll-vs-SSE is an internal detail (v1 polls — simplest, proven).
package bridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/lever-to/lever/internal/scion"
)

// Inboxer is the slice of the scion client the bridge needs (so it's testable
// with a fake). *scion.Client satisfies it.
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

// PollOnce fetches the full inbox, appends every event whose id is unseen as one
// JSON line, marks them seen, and returns the new events.
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
	if err := os.MkdirAll(filepath.Dir(b.file), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(b.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	for _, e := range fresh {
		line, err := json.Marshal(e)
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

// Run polls forever at the given interval (thin loop, not unit-tested).
func (b *Bridge) Run(ctx context.Context, interval time.Duration) error {
	for {
		if _, err := b.PollOnce(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
