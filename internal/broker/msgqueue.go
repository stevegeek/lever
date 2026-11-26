package broker

// The message queue (broker.messaging.queue). A message to an agent that is
// not running (suspended, stopped, or in any other phase but running) is kept
// by the broker and sent when the agent next runs, instead of being refused.
//
// The queue is host-side only: one directory per recipient under the state
// directory (0700), one file per message (0600), each written to a temp file
// and renamed into place. It is never in the tree or in any mount an agent
// sees. With the state directory inside the tree there is no queue and such a
// send is refused, as before.
//
// Queuing widens nothing. A send is authorized when it is made, exactly as a
// live send is, and again at delivery: the sender must still be an identity,
// not revoked, whom the messaging policy lets reach the same recipient, and
// the recipient must still be the same identity (its spawned record and its
// certificate cutoff, which a destroy, purge or recycle moves). A message
// that fails the check is dropped and its sender told.
//
// Delivery is the live send's path: the same marker, the same neutralising of
// the sender's text, and a sent-ledger record written at delivery, so
// message_verify answers the sender's own kind. The line that says the
// message was queued is lever's, after the marker; the sender's text cannot
// forge it (neutraliseMarkers).
//
// A message is delivered at most once. Before its send its file is renamed
// to .sending; after the send it is removed. A .sending file found later is
// a send a crash interrupted: it may have arrived, so it is dropped, never
// sent again, and its sender is told.

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/sentledger"
)

// MessageQueueConfig is broker.messaging.queue as the broker needs it.
type MessageQueueConfig struct {
	// Dir is the host directory (0700, outside the tree) the queue is kept
	// in. "" = off: a send to an agent that is not running is refused.
	Dir string
	// MaxMessages and MaxBytes bound what may wait for one recipient: the
	// count of messages and the bytes of their text. Expiry is how long a
	// message may wait. Zero takes the default.
	MaxMessages int
	MaxBytes    int64
	Expiry      time.Duration
}

const (
	defaultQueueMaxMessages = 20
	defaultQueueMaxBytes    = 256 << 10
	defaultQueueExpiry      = 7 * 24 * time.Hour
	// queueEvery is how often the queue loop looks for recipients that run
	// again and for messages past their expiry.
	queueEvery = 30 * time.Second
	// queueWakeTimeout bounds a wake's resume and the delivery after it.
	queueWakeTimeout = 10 * time.Minute
	// queueWakesAtOnce bounds the wakes under way.
	queueWakesAtOnce = 4
	// queueFileLimit bounds the read of one queue file: a body is at most
	// jailBodyLimit, and JSON escaping can grow it up to six times.
	queueFileLimit = 8 * jailBodyLimit
)

func (c MessageQueueConfig) withDefaults() MessageQueueConfig {
	c.MaxMessages = cmp.Or(c.MaxMessages, defaultQueueMaxMessages)
	c.MaxBytes = cmp.Or(c.MaxBytes, defaultQueueMaxBytes)
	c.Expiry = cmp.Or(c.Expiry, defaultQueueExpiry)
	return c
}

// queuedMsg is one queued message, as its file holds it. Only the broker
// writes it; Body is the sender's text as it was sent, unmarked.
type queuedMsg struct {
	V  int    `json:"v"`
	ID string `json:"id"`
	// Recipient is the recipient's cert CN, Slug its scion name, and
	// Generation its identity's generation when the message was queued
	// (queueGeneration).
	Recipient  string `json:"recipient"`
	Slug       string `json:"slug"`
	Generation string `json:"generation,omitempty"`
	// Sender is the sender's cert CN, or operatorActor for `lever msg send`.
	// To is the address it sent to, resolved again at delivery, and Kind
	// the kind the send would have been recorded with.
	Sender string `json:"sender"`
	// SenderGeneration is the sender's generation when it sent (""
	// for the operator): a message from an earlier life of a name is not
	// that name's now.
	SenderGeneration string    `json:"sender_generation,omitempty"`
	To               string    `json:"to"`
	Kind             string    `json:"kind"`
	Phase            string    `json:"phase"` // the recipient's phase label when queued
	Body             string    `json:"body"`
	Interrupt        bool      `json:"interrupt,omitempty"`
	QueuedAt         time.Time `json:"queued_at"`

	file string // the file's base name
}

const queuedMsgVersion = 1

// Queue file suffixes: a message waiting, and one whose send began.
const (
	queueWaiting = ".msg"
	queueSending = ".sending"
)

// msgQueue is the queue's files. Each recipient's directory is changed only
// under that recipient's lock.
type msgQueue struct {
	cfg   MessageQueueConfig
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newMsgQueue(c MessageQueueConfig) *msgQueue {
	if c.Dir == "" {
		return nil
	}
	return &msgQueue{cfg: c.withDefaults(), locks: map[string]*sync.Mutex{}}
}

func (q *msgQueue) lock(slug string) func() {
	q.mu.Lock()
	l, ok := q.locks[slug]
	if !ok {
		l = &sync.Mutex{}
		q.locks[slug] = l
	}
	q.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// privateDir checks p is a real directory closed to everyone else; create
// makes it (0700) when missing.
func privateDir(p string, create bool) error {
	if create {
		if err := os.Mkdir(p, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is not a private directory; left for the operator", p)
	}
	return nil
}

// dir is slug's queue directory, made when create.
func (q *msgQueue) dir(slug string, create bool) (string, error) {
	if !purgeNameRE.MatchString(slug) {
		return "", fmt.Errorf("%q is not an agent name", slug)
	}
	if create {
		if err := os.MkdirAll(filepath.Dir(q.cfg.Dir), 0o700); err != nil {
			return "", err
		}
	}
	if err := privateDir(q.cfg.Dir, create); err != nil {
		return "", err
	}
	p := filepath.Join(q.cfg.Dir, slug)
	if err := privateDir(p, create); err != nil {
		return "", err
	}
	return p, nil
}

// queueSeq is the order a file name gives, or false for a name that is not
// a queue file.
func queueSeq(name string) (uint64, bool) {
	base := strings.TrimSuffix(strings.TrimSuffix(name, queueWaiting), queueSending)
	if base == name {
		return 0, false
	}
	num, _, ok := strings.Cut(base, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(num, 10, 64)
	return n, err == nil
}

// read lists slug's queue in order: the waiting messages, and the files of
// sends that began and did not end. An unreadable file is removed and
// counted in bad. The caller holds slug's lock.
func (q *msgQueue) read(slug string) (waiting, sending []queuedMsg, bad int, err error) {
	dir, err := q.dir(slug, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, 0, nil
	}
	if err != nil {
		return nil, nil, 0, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, 0, err
	}
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".tmp-") {
			// A write a crash interrupted: never renamed into place.
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		if _, ok := queueSeq(name); !ok {
			continue
		}
		m, rerr := readQueued(filepath.Join(dir, name))
		if rerr != nil || m.Slug != slug {
			_ = os.Remove(filepath.Join(dir, name))
			bad++
			continue
		}
		m.file = name
		if strings.HasSuffix(name, queueSending) {
			sending = append(sending, m)
		} else {
			waiting = append(waiting, m)
		}
	}
	byName := func(x, y queuedMsg) int {
		a, _ := queueSeq(x.file)
		b, _ := queueSeq(y.file)
		return cmp.Compare(a, b)
	}
	slices.SortFunc(waiting, byName)
	slices.SortFunc(sending, byName)
	return waiting, sending, bad, nil
}

func readQueued(p string) (queuedMsg, error) {
	f, err := openRegular(p)
	if err != nil {
		return queuedMsg{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, queueFileLimit+1))
	if err != nil {
		return queuedMsg{}, err
	}
	if len(raw) > queueFileLimit {
		return queuedMsg{}, errors.New("queue file over its limit")
	}
	var m queuedMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		return queuedMsg{}, err
	}
	if m.V != queuedMsgVersion || m.ID == "" || m.Recipient == "" || m.Sender == "" || m.QueuedAt.IsZero() {
		return queuedMsg{}, errors.New("not a queued message")
	}
	return m, nil
}

// queueFullError is the refusal of a message the recipient's queue has no
// room for. sender: the room left is the sender's share that is full, not
// the whole queue.
type queueFullError struct {
	count, maxCount int
	bytes, maxBytes int64
	adding          int64
	sender          bool
}

func (e *queueFullError) Error() string {
	who := "its message queue"
	if e.sender {
		who = "your share of its message queue (one sender may hold at most half of it)"
	}
	if e.count >= e.maxCount {
		return fmt.Sprintf("%s is full (%d messages waiting, the limit is %d)", who, e.count, e.maxCount)
	}
	return fmt.Sprintf("%s has no room for %d more bytes (%d of %d bytes waiting)", who, e.adding, e.bytes, e.maxBytes)
}

func (e *queueFullError) full() bool {
	return e.count >= e.maxCount || e.bytes+e.adding > e.maxBytes
}

func queueBytes(ms []queuedMsg) int64 {
	var n int64
	for _, m := range ms {
		n += int64(len(m.Body))
	}
	return n
}

// add queues m behind slug's waiting messages and returns how many wait,
// m included (for an agent sender other than the manager, how many of its
// own wait: it learns nothing of other senders). A full queue refuses m
// before anything is written. The agents' messages share the caps, and one
// agent may hold at most half of them, so one agent cannot fill a queue
// against everyone else. The operator's notes have caps of their own, so
// agents can never fill a queue against the operator.
func (q *msgQueue) add(m queuedMsg, ownCount bool) (int, error) {
	unlock := q.lock(m.Slug)
	defer unlock()
	waiting, sending, _, err := q.read(m.Slug)
	if err != nil {
		return 0, err
	}
	all := append(slices.Clone(waiting), sending...)
	adding := int64(len(m.Body))
	var pool, mine []queuedMsg
	for _, x := range all {
		if (x.Sender == operatorActor) == (m.Sender == operatorActor) {
			pool = append(pool, x)
		}
		if x.Sender == m.Sender {
			mine = append(mine, x)
		}
	}
	if full := (&queueFullError{count: len(pool), maxCount: q.cfg.MaxMessages, bytes: queueBytes(pool), maxBytes: q.cfg.MaxBytes, adding: adding}); full.full() {
		return 0, full
	}
	if m.Sender != operatorActor {
		share := &queueFullError{count: len(mine), maxCount: max(q.cfg.MaxMessages/2, 1), bytes: queueBytes(mine), maxBytes: max(q.cfg.MaxBytes/2, 1), adding: adding, sender: true}
		if share.full() {
			return 0, share
		}
	}
	var next uint64 = 1
	for _, x := range all {
		if n, _ := queueSeq(x.file); n >= next {
			next = n + 1
		}
	}
	dir, err := q.dir(m.Slug, true)
	if err != nil {
		return 0, err
	}
	m.V = queuedMsgVersion
	raw, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(dir, fmt.Sprintf("%020d-%s%s", next, m.ID, queueWaiting)), raw, 0o600); err != nil {
		return 0, err
	}
	if ownCount {
		n := 1
		for _, x := range waiting {
			if x.Sender == m.Sender {
				n++
			}
		}
		return n, nil
	}
	return len(waiting) + 1, nil
}

// remove deletes one message's file. The caller holds its recipient's lock.
func (q *msgQueue) remove(m queuedMsg) error {
	dir, err := q.dir(m.Slug, false)
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, m.file))
}

// beginSend renames m's file to .sending and returns m as renamed.
func (q *msgQueue) beginSend(m queuedMsg) (queuedMsg, error) {
	dir, err := q.dir(m.Slug, false)
	if err != nil {
		return m, err
	}
	to := strings.TrimSuffix(m.file, queueWaiting) + queueSending
	if err := os.Rename(filepath.Join(dir, m.file), filepath.Join(dir, to)); err != nil {
		return m, err
	}
	m.file = to
	return m, nil
}

// undoSend renames a .sending file back: the send was refused before
// anything left, so the message waits again in its place.
func (q *msgQueue) undoSend(m queuedMsg) error {
	dir, err := q.dir(m.Slug, false)
	if err != nil {
		return err
	}
	return os.Rename(filepath.Join(dir, m.file), filepath.Join(dir, strings.TrimSuffix(m.file, queueSending)+queueWaiting))
}

// count is how many messages wait for slug, 0 when none or unreadable. It
// reads names only.
func (q *msgQueue) count(slug string) int {
	if q == nil || !purgeNameRE.MatchString(slug) {
		return 0
	}
	ents, err := os.ReadDir(filepath.Join(q.cfg.Dir, slug))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if _, ok := queueSeq(e.Name()); ok {
			n++
		}
	}
	return n
}

// recipients lists the names that have a queue directory.
func (q *msgQueue) recipients() []string {
	ents, err := os.ReadDir(q.cfg.Dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && purgeNameRE.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

func newQueueID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// queueGeneration is the generation of the identity named slug: its
// spawned record's creation and its certificate cutoff. A destroy draws no
// name again, and a purge or recycle moves the cutoff (Supersede), so a
// message queued for one life of a name is never delivered to the next.
func (b *Broker) queueGeneration(cn, slug string) string {
	var g []string
	if rec, ok := b.workers.record(slug); ok {
		g = append(g, "created="+rec.CreatedAt.UTC().Format(time.RFC3339Nano))
	}
	if t := b.leafFrom(cn); !t.IsZero() {
		g = append(g, "cutoff="+t.UTC().Format(time.RFC3339Nano))
	}
	return strings.Join(g, ";")
}

// senderGeneration is the generation of the agent identity cn, "" for the
// operator.
func (b *Broker) senderGeneration(cn string) string {
	if cn == operatorActor {
		return ""
	}
	_, slug, _, ok := b.identity(cn)
	if !ok {
		return ""
	}
	return b.queueGeneration(cn, slug)
}

// queuedSend is a send the handler could not make because the recipient is
// not running.
type queuedSend struct {
	caller, to        string
	recipientCN, slug string
	kind, phase, body string
	interrupt, wake   bool
}

// queueAnswer is what a queued send answers.
type queueAnswer struct {
	waiting int
	waking  bool
	note    string
}

// errQueueOff: there is no queue (the state directory is inside the tree).
var errQueueOff = errors.New("no message queue")

// enqueue queues s and, with wake, starts the recipient's resume when the
// caller may resume it and admission lets it start; otherwise note says why
// it was not woken. A *queueFullError means nothing was queued.
func (b *Broker) enqueue(ctx context.Context, s queuedSend) (queueAnswer, error) {
	if b.queue == nil {
		return queueAnswer{}, errQueueOff
	}
	id, err := newQueueID()
	if err != nil {
		return queueAnswer{}, err
	}
	m := queuedMsg{ID: id, Recipient: s.recipientCN, Slug: s.slug, Generation: b.queueGeneration(s.recipientCN, s.slug),
		Sender: s.caller, SenderGeneration: b.senderGeneration(s.caller), To: s.to, Kind: s.kind, Phase: s.phase, Body: s.body, Interrupt: s.interrupt, QueuedAt: b.now().UTC()}
	n, err := b.queue.add(m, s.caller != operatorActor && s.caller != b.manager)
	if err != nil {
		return queueAnswer{}, err
	}
	b.audit("msg", s.caller, "allow", "queue->agent:"+s.slug+" (phase "+s.phase+")", "kind", s.kind, "queue_id", id, "waiting", n)
	ans := queueAnswer{waiting: n}
	if s.wake {
		if why := b.wakeRefusal(ctx, s.caller, s.slug); why != "" {
			b.audit("msg", s.caller, "deny", "wake agent:"+s.slug+": "+why)
			ans.note = "not woken: " + why
		} else if b.startWake(s.caller, s.slug) {
			ans.waking = true
		} else {
			ans.note = "not woken: too many wakes are under way; it is delivered when the agent next runs"
		}
	}
	return ans, nil
}

// wakeRefusal says why caller may not wake slug now, or "" when it may: the
// same rule as who may resume it (the manager its reachable workers, the
// operator any declared worker, a worker no one), and admission (the
// running caps and the memory budget) must let it start now.
func (b *Broker) wakeRefusal(ctx context.Context, caller, slug string) string {
	if slug == b.managerSlug {
		return "the manager is brought back by the operator with `lever up`"
	}
	spec, ok := b.workerSpec(slug)
	switch {
	case !ok:
		return "it is not a declared worker"
	case caller == operatorActor:
	case caller == b.manager && b.managerMayReach(spec.Name):
	default:
		return "only the manager and the operator may wake an agent"
	}
	if b.isRevoked(spec.Name) {
		return "it is revoked"
	}
	release, err := b.admitWorker(ctx, spec.Name)
	if err == nil {
		release()
		return ""
	}
	_, msg, _ := b.admissionRefusal(err, caller)
	return msg
}

// startWake resumes slug in the background as caller would, then delivers
// its queue. A wake that fails leaves the queue as it is, and the agents
// whose messages wait for slug are told why.
func (b *Broker) startWake(caller, slug string) bool {
	select {
	case b.wakeSem <- struct{}{}:
	default:
		return false
	}
	b.queueJobs.Add(1)
	go func() {
		defer b.queueJobs.Done()
		defer func() { <-b.wakeSem }()
		ctx, cancel := context.WithTimeout(b.background(), queueWakeTimeout)
		defer cancel()
		spec, ok := b.workerSpec(slug)
		if !ok {
			return
		}
		cw := newCaptureWriter()
		b.resumeWorker(ctx, cw, spec, caller)
		if cw.code != 0 && cw.code != 200 {
			why := strings.TrimSpace(cw.body.String())
			if len(why) > 600 {
				why = strings.ToValidUTF8(why[:600], "") + "…"
			}
			b.audit("msg", caller, "error", "wake agent:"+slug+": the resume was refused, its messages stay queued")
			b.queueNoticeTo(ctx, caller, "Lever could not wake "+slug+" to deliver the message you queued for it: "+why+". The message stays queued and goes out when "+slug+" next runs.")
			return
		}
		b.deliverWhenFree(ctx, slug, true)
	}()
	return true
}

// background is the context of the broker's background work: the serving
// context once ServeListeners runs, else (tests) one never cancelled.
func (b *Broker) background() context.Context {
	b.bgMu.Lock()
	defer b.bgMu.Unlock()
	if b.bgCtx == nil {
		return context.Background()
	}
	return b.bgCtx
}

// deliverWhenFree delivers slug's queue under its worker lock: waiting for
// it when wait, else skipping a worker whose lock is held (a verb under
// way; the next pass tries again). The manager has no worker lock.
func (b *Broker) deliverWhenFree(ctx context.Context, slug string, wait bool) {
	if b.queue == nil {
		return
	}
	if slug != b.managerSlug {
		var unlock func()
		if wait {
			u, err := b.lockWorker(ctx, slug)
			if err != nil {
				return
			}
			unlock = u
		} else {
			u, ok := b.tryLockWorker(slug)
			if !ok {
				return
			}
			unlock = u
		}
		defer unlock()
	}
	b.deliverQueued(ctx, slug)
}

// afterResume delivers name's queue in the background once a resume or wake
// brought it up. The resume's caller holds the worker lock, which the
// delivery waits for.
func (b *Broker) afterResume(name string) {
	if b.queue == nil || b.queue.count(name) == 0 {
		return
	}
	b.queueJobs.Add(1)
	go func() {
		defer b.queueJobs.Done()
		ctx, cancel := context.WithTimeout(b.background(), queueWakeTimeout)
		defer cancel()
		b.deliverWhenFree(ctx, name, true)
	}()
}

// queuedStillAllowed applies the messaging policy to m again: a reason to
// drop it, or "" to deliver it.
func (b *Broker) queuedStillAllowed(m queuedMsg) string {
	cn, slug, _, ok := b.identity(m.Slug)
	if !ok || cn != m.Recipient || slug != m.Slug {
		return "the recipient is no longer an agent of this instance"
	}
	if b.queueGeneration(cn, slug) != m.Generation {
		return "the recipient was destroyed, purged or recycled since it was queued"
	}
	if m.Sender == operatorActor {
		if m.Kind != sentledger.KindOperatorNote {
			return "it is not a message lever queued"
		}
		return ""
	}
	if b.isRevoked(m.Sender) {
		return "its sender is revoked"
	}
	if b.senderGeneration(m.Sender) != m.SenderGeneration {
		return "its sender was destroyed, purged or recycled since it was sent"
	}
	tgt, err := b.resolveMsgTarget(m.Sender, m.To)
	if err != nil || tgt.recipientCN != m.Recipient {
		return "the messaging policy no longer lets its sender reach the recipient"
	}
	if queuedKind(tgt) != m.Kind {
		return "its sender's kind changed"
	}
	return ""
}

// queuedKind is the kind a send to tgt is recorded with: from the caller's
// identity, never the request.
func queuedKind(tgt msgTarget) string {
	if tgt.relayFrom != "" {
		return sentledger.WorkerKind(tgt.relayFrom)
	}
	return sentledger.KindManager
}

// queuedMarker is the marker of a message of kind, as a live send of it has.
func queuedMarker(kind string) string {
	switch kind {
	case sentledger.KindOperatorNote:
		return operatorNoteMarker
	case sentledger.KindManager:
		return managerMarker
	}
	return fmt.Sprintf(relayMarkerFormat, strings.TrimPrefix(kind, sentledger.KindWorkerPrefix))
}

// queuedLine is lever's line on a delivered message: when it was sent and
// that it waited.
func queuedLine(m queuedMsg) string {
	was := "not running"
	if m.Phase == scion.PhaseSuspended || m.Phase == scion.PhaseStopped {
		was = m.Phase
	}
	return "[lever: sent at " + m.QueuedAt.UTC().Format(time.RFC3339) + ", queued while you were " + was + "]"
}

func queuedBody(m queuedMsg, ref string) string {
	return refLine(queuedMarker(m.Kind), ref) + "\n" + queuedLine(m) + "\n" + neutraliseMarkers(m.Body)
}

// expired reports m past the queue's expiry at now.
func (q *msgQueue) expired(m queuedMsg, now time.Time) bool {
	return now.Sub(m.QueuedAt) >= q.cfg.Expiry
}

// dropQueued removes m and tells its sender why. The caller holds m's
// recipient's lock.
func (b *Broker) dropQueued(ctx context.Context, m queuedMsg, why string) {
	if err := b.queue.remove(m); err != nil && !errors.Is(err, fs.ErrNotExist) {
		b.audit("msg", m.Sender, "error", "queue->agent:"+m.Slug+": removing "+m.ID+": "+err.Error())
	}
	b.audit("msg", m.Sender, "deny", "queue->agent:"+m.Slug+": dropped: "+why, "queue_id", m.ID)
	b.queueNoticeTo(ctx, m.Sender, fmt.Sprintf("Your message to %s, queued at %s, was dropped and not delivered: %s.",
		m.Slug, m.QueuedAt.UTC().Format(time.RFC3339), why))
}

// sweepLocked drops slug's interrupted sends, its expired messages and the
// ones the policy no longer allows, and returns the rest in order. The
// caller holds slug's lock.
func (b *Broker) sweepLocked(ctx context.Context, slug string) []queuedMsg {
	waiting, sending, bad, err := b.queue.read(slug)
	if err != nil {
		b.audit("msg", "lever", "error", "queue of agent:"+slug+": "+err.Error())
		return nil
	}
	if bad > 0 {
		b.audit("msg", "lever", "error", fmt.Sprintf("queue of agent:%s: removed %d unreadable file(s)", slug, bad))
	}
	for _, m := range sending {
		b.dropQueued(ctx, m, "lever was stopped while sending it, so it may or may not have arrived; it is not sent again")
	}
	now := b.now()
	var keep []queuedMsg
	for _, m := range waiting {
		if b.queue.expired(m, now) {
			b.dropQueued(ctx, m, "it waited "+b.queue.cfg.Expiry.String()+", the queue's expiry, and the recipient did not run")
			continue
		}
		if why := b.queuedStillAllowed(m); why != "" {
			b.dropQueued(ctx, m, why)
			continue
		}
		keep = append(keep, m)
	}
	return keep
}

// deliverQueued sends slug's queue in order while slug runs, each message on
// the live send's path. It stops at the first message it could not send,
// which waits in its place. The caller holds slug's worker lock when slug is
// a worker.
func (b *Broker) deliverQueued(ctx context.Context, slug string) (sent int) {
	unlock := b.queue.lock(slug)
	defer unlock()
	pending := b.sweepLocked(ctx, slug)
	if len(pending) == 0 {
		return 0
	}
	if b.isRevoked(pending[0].Recipient) {
		return 0 // held until it is no longer revoked, or expires
	}
	toManager := slug == b.managerSlug
	if _, _, phase := b.notRunningCheck(ctx, "lever", slug, toManager); phase != scion.PhaseRunning {
		return 0
	}
	for i, m := range pending {
		// Read again before each send but the first: the fewer sends that
		// meet an agent gone away, the fewer are dropped below.
		if i > 0 {
			if _, _, phase := b.notRunningCheck(ctx, "lever", slug, toManager); phase != scion.PhaseRunning {
				return sent
			}
		}
		m, err := b.queue.beginSend(m)
		if err != nil {
			b.audit("msg", m.Sender, "error", "queue->agent:"+slug+": "+err.Error())
			return sent
		}
		ref, err := b.sendRecorded(ctx, m.Recipient, slug, m.Kind, func(ref string) string { return queuedBody(m, ref) },
			scion.MsgOpts{To: "agent:" + slug, Interrupt: m.Interrupt, Project: b.instanceProject})
		if errors.Is(err, errNotRecorded) {
			// Nothing left: the message waits again.
			if uerr := b.queue.undoSend(m); uerr != nil {
				b.audit("msg", m.Sender, "error", "queue->agent:"+slug+": "+uerr.Error())
			}
			return sent
		}
		if err != nil {
			b.audit("msg", m.Sender, "error", "queued send->agent:"+slug+" "+ref+": "+err.Error())
			// A runtime error cannot tell a refusal from a send that arrived
			// (a timeout after the hub took it), so the message is never
			// sent again. The rest wait in order.
			b.dropQueued(ctx, m, "the send failed with a runtime error after it began, so it may or may not have arrived; it is not sent again")
			return sent
		}
		if err := b.queue.remove(m); err != nil {
			b.audit("msg", m.Sender, "error", "queue->agent:"+slug+": removing "+m.ID+" after its send: "+err.Error())
		}
		b.audit("msg", m.Sender, "allow", "queued send->agent:"+slug, "kind", m.Kind, "ref", ref, "queue_id", m.ID)
		sent++
	}
	return sent
}

// dropQueue drops every message waiting for slug and tells their senders
// why: its recipient is gone.
func (b *Broker) dropQueue(ctx context.Context, slug, why string) {
	if b.queue == nil || b.queue.count(slug) == 0 {
		return
	}
	unlock := b.queue.lock(slug)
	defer unlock()
	waiting, sending, _, err := b.queue.read(slug)
	if err != nil {
		b.audit("msg", "lever", "error", "queue of agent:"+slug+": "+err.Error())
		return
	}
	for _, m := range append(sending, waiting...) {
		b.dropQueued(ctx, m, why)
	}
	if dir, err := b.queue.dir(slug, false); err == nil {
		_ = os.Remove(dir) // only when empty
	}
}

// queueNoticeMarker marks lever's notice to a sender about a message it
// queued (sentledger.KindQueueNotice).
const queueNoticeMarker = "[lever: message queue notice]"

// queueNoticeTo tells sender, when it is an agent that runs now, what became
// of a message it queued. It is never queued itself; a notice that cannot go
// out is in the audit log only, as every notice is.
func (b *Broker) queueNoticeTo(ctx context.Context, sender, text string) {
	b.audit("msg", "lever", "allow", "queue notice for "+sender+": "+text)
	if sender == operatorActor || b.runtime == nil {
		return
	}
	cn, slug, isManager, ok := b.identity(sender)
	if !ok || cn != sender || b.isRevoked(cn) {
		return
	}
	if _, _, phase := b.notRunningCheck(ctx, "lever", slug, isManager); phase != scion.PhaseRunning {
		b.audit("msg", "lever", "deny", "queue notice for "+sender+": not sent, the sender is not running")
		return
	}
	ref, err := b.sendRecorded(ctx, cn, slug, sentledger.KindQueueNotice, func(ref string) string { return refLine(queueNoticeMarker, ref) + "\n" + text },
		scion.MsgOpts{To: "agent:" + slug, Project: b.instanceProject})
	if err != nil {
		b.audit("msg", "lever", "error", "queue notice for "+sender+" "+ref+": "+err.Error())
	}
}

// runQueueLoop delivers to recipients that run again and drops expired
// messages, every queueEvery.
func (b *Broker) runQueueLoop(ctx context.Context) {
	t := time.NewTicker(cmp.Or(b.queueEvery, queueEvery))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.queuePass(ctx)
		}
	}
}

// queuePass is one pass of the queue loop. It reads the hub only when a
// queue is not empty.
func (b *Broker) queuePass(ctx context.Context) {
	var slugs []string
	for _, slug := range b.queue.recipients() {
		unlock := b.queue.lock(slug)
		if len(b.sweepLocked(ctx, slug)) > 0 {
			slugs = append(slugs, slug)
		} else if dir, err := b.queue.dir(slug, false); err == nil {
			_ = os.Remove(dir) // only when empty
		}
		unlock()
	}
	if len(slugs) == 0 {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, agentLoopListTimeout)
	agents, err := b.runtime.List(lctx, b.instanceProject)
	cancel()
	if err != nil {
		return
	}
	for _, slug := range slugs {
		if a := scion.FindAgent(agents, slug); a != nil && scion.PhaseLabel(a.Phase) == scion.PhaseRunning && !scion.RunningContainerDown(scion.PhaseRunning, a.ContainerStatus) {
			b.deliverWhenFree(ctx, slug, false)
		}
	}
}

func (b *Broker) queueLoopEnabled() bool {
	return b.queue != nil && b.runtime != nil
}

// queueRefusal is the answer to a send that could not be queued: the
// recipient's not-running refusal and why. The detail of an error that is
// not a full queue stays in the audit log.
func (b *Broker) queueRefusal(caller, refusal string, err error) string {
	if full := (*queueFullError)(nil); errors.As(err, &full) {
		return refusal + "; " + full.Error() + ", so nothing was queued"
	}
	b.audit("msg", caller, "error", "queue: "+err.Error())
	return refusal + "; the message queue could not take it (queue error), so nothing was queued"
}

// phaseToken is a phase label as one token for an answer.
func phaseToken(phase string) string {
	if phase == phaseRunningContainerDown {
		return PhaseRunningContainerDownToken
	}
	return phase
}

// PathOperatorMsgQueue is `lever msg list`'s route on the operator socket:
// the queued messages, of one recipient with ?to=NAME.
const PathOperatorMsgQueue = "/operator/msg/queue"

// QueueEntry is one queued message as the operator sees it. Body is the
// sender's text, unmarked: a reader must treat it as the sender's word.
type QueueEntry struct {
	ID string `json:"id"`
	// To is the recipient's name; Kind who sent it, as the sent ledger will
	// record it (worker:<slug>, manager, operator-note).
	To        string    `json:"to"`
	Kind      string    `json:"kind"`
	QueuedAt  time.Time `json:"queued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Bytes     int       `json:"bytes"`
	Interrupt bool      `json:"interrupt,omitempty"`
	// Sending: its send began and did not end (it will be dropped).
	Sending bool   `json:"sending,omitempty"`
	Body    string `json:"body"`
}

// QueueListResponse answers PathOperatorMsgQueue. Off: there is no queue
// (the state directory is inside the tree).
type QueueListResponse struct {
	Off         bool         `json:"off,omitempty"`
	MaxMessages int          `json:"max_messages,omitempty"`
	MaxBytes    int64        `json:"max_bytes,omitempty"`
	Expiry      string       `json:"expiry,omitempty"`
	Messages    []QueueEntry `json:"messages"`
}

func (b *Broker) handleOperatorMsgQueue(w http.ResponseWriter, r *http.Request) {
	out := QueueListResponse{Messages: []QueueEntry{}}
	if b.queue == nil {
		out.Off = true
		writeJSON(w, out)
		return
	}
	out.MaxMessages, out.MaxBytes, out.Expiry = b.queue.cfg.MaxMessages, b.queue.cfg.MaxBytes, b.queue.cfg.Expiry.String()
	names := b.queue.recipients()
	if to := r.URL.Query().Get("to"); to != "" {
		if to == "manager" || to == b.manager {
			to = b.managerSlug
		}
		if _, _, _, ok := b.identity(to); !ok {
			refuse(w, ReasonNotAWorker, "unknown agent", http.StatusNotFound)
			return
		}
		names = []string{to}
	}
	slices.Sort(names)
	for _, slug := range names {
		unlock := b.queue.lock(slug)
		waiting, sending, _, err := b.queue.read(slug)
		unlock()
		if err != nil {
			b.audit("msg", operatorActor, "error", "queue list "+slug+": "+err.Error())
			refuse(w, ReasonRuntimeError, "queue error", http.StatusInternalServerError)
			return
		}
		for _, m := range append(sending, waiting...) {
			out.Messages = append(out.Messages, QueueEntry{ID: m.ID, To: m.Slug, Kind: m.Kind, QueuedAt: m.QueuedAt,
				ExpiresAt: m.QueuedAt.Add(b.queue.cfg.Expiry), Bytes: len(m.Body), Interrupt: m.Interrupt,
				Sending: strings.HasSuffix(m.file, queueSending), Body: m.Body})
		}
	}
	writeJSON(w, out)
}
