package broker

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/sentledger"
)

// sentRecord opens the sent ledger on first use and again after a failure,
// so a directory fixed while the broker runs is picked up.
type sentRecord struct {
	dir string // "" = off: the state directory is inside the tree

	mu     sync.Mutex
	ledger *sentledger.Ledger
}

var errSentOff = errors.New("the sent ledger is off: the state directory is inside the tree")

func (s *sentRecord) get(now time.Time) (*sentledger.Ledger, error) {
	if s.dir == "" {
		return nil, errSentOff
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ledger == nil {
		l, err := sentledger.Open(s.dir, now)
		if err != nil {
			return nil, err
		}
		s.ledger = l
	}
	return s.ledger, nil
}

// sendTimeout lets a send with no done line be trusted to have ended within
// sentledger.MaxSendDuration of its start.
const sendTimeout = time.Minute

// errNotRecorded: a send that cannot be recorded is not made, since it could
// never verify.
var errNotRecorded = errors.New("cannot record the message")

// refLine is the first line of every lever message: the marker (a hint, never
// evidence) and the ref message_verify looks it up by.
func refLine(marker, ref string) string {
	if ref == "" {
		return marker
	}
	return marker + " ref=" + ref
}

// sendRecorded records a send, then makes it; a send whose record cannot be
// written is not made. A scion error still leaves a verifiable record: the
// hub may have delivered the bytes. With the ledger off the send goes out
// unrecorded and every verify answers unavailable.
func (b *Broker) sendRecorded(ctx context.Context, recipientCN, slug, kind string, compose func(ref string) string, o scion.MsgOpts) (string, error) {
	now := time.Now()
	led, err := b.sent.get(now)
	if errors.Is(err, errSentOff) {
		o.Body = compose("")
		b.audit("msg", recipientCN, "error", "send "+kind+" unrecorded: "+err.Error())
		return "", b.runtime.Message(ctx, o)
	}
	if err != nil {
		b.audit("msg", recipientCN, "error", "send "+kind+": "+err.Error())
		return "", errNotRecorded
	}
	id, err := sentledger.NewID()
	if err != nil {
		b.audit("msg", recipientCN, "error", "send "+kind+": "+err.Error())
		return "", errNotRecorded
	}
	o.Body = compose(id)
	e := sentledger.Sent{ID: id, Recipient: recipientCN, Slug: slug, Kind: kind, Body: o.Body, Before: time.Now()}
	if err := led.Begin(e); err != nil {
		b.audit("msg", recipientCN, "error", "send "+kind+": "+err.Error())
		return "", errNotRecorded
	}
	mctx, cancel := context.WithTimeout(ctx, sendTimeout)
	merr := b.runtime.Message(mctx, o)
	cancel()
	if err := led.Done(e, time.Now(), merr == nil); err != nil {
		// The entry stays in flight, bounded by MaxSendDuration.
		b.audit("msg", recipientCN, "error", "send "+kind+" "+id+": recording the end: "+err.Error())
	}
	return id, merr
}
