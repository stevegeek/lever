package broker

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/sentledger"
)

// sentRecord is the broker's handle on the sent ledger (package sentledger):
// the host record of every message lever sends to an agent, which
// /message/verify answers from. It opens the ledger on first use and again
// after a failure, so a directory fixed while the broker runs is picked up.
type sentRecord struct {
	dir string // "" = off: the state directory is inside the tree

	mu     sync.Mutex
	ledger *sentledger.Ledger
}

// errSentOff means the sent ledger is off, because the state directory is
// inside the tree an agent can write.
var errSentOff = errors.New("the sent ledger is off: the state directory is inside the tree")

// get returns the open ledger, opening it if needed.
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

// sendTimeout bounds one recorded send's scion call, so a send with no done
// line (the broker stopped mid-call) can be trusted to have ended within
// sentledger.MaxSendDuration of its start.
const sendTimeout = time.Minute

// errNotRecorded means the broker could not record a send, so it did not
// make it: an unrecorded lever message could never verify, and an agent
// would treat it as data.
var errNotRecorded = errors.New("cannot record the message")

// refLine is the first line of every message lever sends: its marker (a
// readable hint only, never evidence) and the send's ref, the key the
// recipient's message_verify call looks the message up by.
func refLine(marker, ref string) string {
	if ref == "" {
		return marker
	}
	return marker + " ref=" + ref
}

// sendRecorded records a send in the sent ledger and makes it. compose
// builds the body from the send's ref. The record comes first, and a send
// whose record cannot be written is not made (errNotRecorded), the way
// chatUses.take grants no use it cannot write. A scion error still leaves a
// verifiable record: lever wrote those bytes, and the hub may have delivered
// them (a dispatch retry that timed out).
//
// With the ledger off (the state directory inside the tree) the send goes
// out unrecorded and without a ref: every verify then answers unavailable,
// which doctor reports.
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
		// The entry stays in flight, bounded by sentledger.MaxSendDuration
		// for the time-window fallback; a ref lookup is not affected.
		b.audit("msg", recipientCN, "error", "send "+kind+" "+id+": recording the end: "+err.Error())
	}
	return id, merr
}
