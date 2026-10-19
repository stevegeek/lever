// Package bridge is the poll-based notification bridge: it appends each new
// Scion agent event (by id) as one JSON line to an events file, which the Lever
// manager watches with a Monitor. The file is the fixed interface; polling is
// an internal detail.
package bridge

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/scion"
)

// defaultMaxBackoff caps the wait between polls while the inbox keeps failing.
const defaultMaxBackoff = time.Minute

// Inboxer is the slice of an inbox source the bridge needs.
type Inboxer interface {
	Inbox(ctx context.Context, unread bool, project string) ([]scion.Event, error)
}

type Bridge struct {
	in   Inboxer
	file string
	seen map[string]bool
	// Log receives one line per failed poll that Run retries, and one when the
	// inbox answers again. New sets it to os.Stderr.
	Log io.Writer
	// MaxBackoff caps the wait between retries; it is never below the poll
	// interval. New sets it to one minute.
	MaxBackoff time.Duration
}

func New(in Inboxer, eventsFile string) *Bridge {
	return &Bridge{in: in, file: eventsFile, seen: map[string]bool{}, Log: os.Stderr, MaxBackoff: defaultMaxBackoff}
}

// inboxError marks an error from the inbox source, as opposed to one from the
// events file, so Run can tell which failures may pass on their own.
type inboxError struct{ err error }

func (e inboxError) Error() string { return e.err.Error() }
func (e inboxError) Unwrap() error { return e.err }

// transient reports whether a failed poll may succeed if tried again: an inbox
// request the broker did not answer (a transport error, not a TLS certificate
// refusal), or an answer of 5xx, 408 or 429. Everything else fails the same
// way next time and is returned: a 4xx refusal, a bad bootstrap or identity, a
// certificate either side rejects, a body that does not decode, and an
// events-file error.
func transient(err error) bool {
	var ie inboxError
	if !errors.As(err, &ie) {
		return false
	}
	if code := httpjson.Status(err); code != 0 {
		return code >= 500 || code == http.StatusRequestTimeout || code == http.StatusTooManyRequests
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		return false
	}
	var cve *tls.CertificateVerificationError
	var alert tls.AlertError
	return !errors.As(err, &cve) && !errors.As(err, &alert)
}

// maxLogErr bounds an error in a log line: a 5xx body can be a proxy's whole
// HTML page.
const maxLogErr = 300

func logErr(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	if len(s) > maxLogErr {
		s = strings.ToValidUTF8(s[:maxLogErr], "") + "…"
	}
	return s
}

// PollOnce appends every event with an unseen id to the events file and
// returns the new events.
func (b *Bridge) PollOnce(ctx context.Context) ([]scion.Event, error) {
	events, err := b.in.Inbox(ctx, false, "")
	if err != nil {
		return nil, inboxError{err}
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

// Run polls forever at the given interval. A transient failure (see
// transient) is logged and retried, waiting twice as long after each one in a
// row, up to MaxBackoff, and the interval again once a poll succeeds. Run
// returns on any other error, or when ctx ends.
func (b *Bridge) Run(ctx context.Context, interval time.Duration) error {
	wait, failed := interval, 0
	for {
		_, err := b.PollOnce(ctx)
		switch {
		case err == nil:
			if failed > 0 {
				b.logf("watch: inbox answered again after %d failed %s", failed, plural(failed, "poll"))
			}
			wait, failed = interval, 0
		case ctx.Err() != nil:
			return ctx.Err()
		case !transient(err):
			return err
		default:
			failed++
			if failed > 1 {
				wait = min(2*wait, max(interval, b.MaxBackoff))
			}
			b.logf("watch: poll failed (%d in a row), retrying in %s: %s", failed, wait, logErr(err))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func (b *Bridge) logf(format string, args ...any) {
	if b.Log != nil {
		fmt.Fprintf(b.Log, format+"\n", args...)
	}
}
