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
// to .sending; after the send it is removed (or, when the remove fails,
// renamed to .sent, which is removed later without a word). A .sending file
// found later, of no send this broker has under way, is a send a crash
// interrupted: it may have arrived, so it is dropped, never sent again, and
// its sender is told.
//
// A recipient's queue lock is held only while its files are read or
// changed, never across a hub read or a send, so a send queued for it and
// `lever msg list` never wait on a delivery. Deliveries to one recipient are
// serial under its delivery guard (msgQueue.delivering), and each takes the
// lowest waiting message next, so a message queued while a delivery runs
// goes out after the ones before it, in that delivery or the next.
//
// What becomes of a queued message is told to its sender by a queue notice.
// Notices are never sent under a queue lock or a worker lock: the code under
// the locks only decides and changes files, and hands the notices to one
// background sender (queueNotice), which holds at most queueNoticesPending.

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
	"syscall"
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
	// MaxMessages and MaxBytes bound each pool of one recipient's queue (the
	// agents' messages, the manager's, and the operator's notes): the count
	// of messages and the bytes of their text. Expiry is how long a message
	// may wait. Zero takes the default.
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
	// queueNoticesPending bounds the notices waiting for the background
	// sender; past it a notice is in the audit log only.
	queueNoticesPending = 256
	// queueNoticeTimeout bounds one notice: the phase read and the send.
	queueNoticeTimeout = msgPhaseTimeoutDefault + sendTimeout
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

// Queue file suffixes: a message waiting, one whose send began, and one
// sent whose file could not be removed.
const (
	queueWaiting = ".msg"
	queueSending = ".sending"
	queueSent    = ".sent"
)

// msgQueue is the queue's files. Each recipient's directory is changed only
// under that recipient's lock.
type msgQueue struct {
	cfg   MessageQueueConfig
	mu    sync.Mutex
	locks map[string]*sync.Mutex
	// delivering holds each recipient's delivery guard: a channel of one
	// slot, so a delivery can wait for it under a context or skip it.
	delivering map[string]chan struct{}
	// inSend holds the ids of the messages whose send is under way: their
	// .sending file is no interrupted send. Under mu.
	inSend map[string]bool
	// delivered holds the ids of messages sent whose file could be neither
	// removed nor renamed to .sent, so this broker drops what is left of
	// them without a word. An id goes when its file does. Under mu.
	delivered map[string]bool
	// unreadable holds the recipients whose queue could not be read at the
	// last sweep, so that is audited once, not every pass. Under mu.
	unreadable map[string]bool
	// removeFile removes a file (os.Remove; a test makes it fail).
	removeFile func(string) error
}

func newMsgQueue(c MessageQueueConfig) *msgQueue {
	if c.Dir == "" {
		return nil
	}
	return &msgQueue{cfg: c.withDefaults(), locks: map[string]*sync.Mutex{}, delivering: map[string]chan struct{}{},
		inSend: map[string]bool{}, delivered: map[string]bool{}, unreadable: map[string]bool{}, removeFile: os.Remove}
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

// guard is slug's delivery guard.
func (q *msgQueue) guard(slug string) chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	g, ok := q.delivering[slug]
	if !ok {
		g = make(chan struct{}, 1)
		q.delivering[slug] = g
	}
	return g
}

// waitDelivery takes slug's delivery guard, waiting for a delivery under
// way, or fails when ctx ends first.
func (q *msgQueue) waitDelivery(ctx context.Context, slug string) (func(), error) {
	g := q.guard(slug)
	select {
	case g <- struct{}{}:
		return func() { <-g }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// tryDelivery takes slug's delivery guard when no delivery is under way.
func (q *msgQueue) tryDelivery(slug string) (func(), bool) {
	g := q.guard(slug)
	select {
	case g <- struct{}{}:
		return func() { <-g }, true
	default:
		return nil, false
	}
}

// sendUnderWay reports m's send under way in this broker.
func (q *msgQueue) sendUnderWay(m queuedMsg) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.inSend[m.ID]
}

// readFailed records whether slug's queue could be read, and reports a
// failure that is new: one to audit.
func (q *msgQueue) readFailed(slug string, failed bool) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	was := q.unreadable[slug]
	if failed {
		q.unreadable[slug] = true
	} else {
		delete(q.unreadable, slug)
	}
	return failed && !was
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
// a message waiting or being sent.
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
// sends that began and did not end. A file that is not a queued message of
// slug's is removed and counted in bad; one that could not be read (an I/O
// error, which may pass) is left and counted in skipped. A .sent file is not
// read. The caller holds slug's lock.
func (q *msgQueue) read(slug string) (waiting, sending []queuedMsg, bad, skipped int, err error) {
	dir, err := q.dir(slug, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, 0, 0, nil
	}
	if err != nil {
		return nil, nil, 0, 0, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, 0, 0, err
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
		if rerr != nil && !errors.Is(rerr, errBadQueueFile) {
			skipped++
			continue
		}
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
	return waiting, sending, bad, skipped, nil
}

// errBadQueueFile: a file that is not a queued message (not a regular file,
// over the limit, not JSON, or not one lever wrote). Any other error of
// readQueued is an I/O error.
var errBadQueueFile = errors.New("not a queued message")

func readQueued(p string) (queuedMsg, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return queuedMsg{}, fmt.Errorf("%w: a symlink", errBadQueueFile)
	}
	if err != nil {
		return queuedMsg{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return queuedMsg{}, err
	}
	if !fi.Mode().IsRegular() {
		return queuedMsg{}, fmt.Errorf("%w: not a regular file", errBadQueueFile)
	}
	raw, err := io.ReadAll(io.LimitReader(f, queueFileLimit+1))
	if err != nil {
		return queuedMsg{}, err
	}
	if len(raw) > queueFileLimit {
		return queuedMsg{}, fmt.Errorf("%w: over the limit", errBadQueueFile)
	}
	var m queuedMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		return queuedMsg{}, fmt.Errorf("%w: %v", errBadQueueFile, err)
	}
	if m.V != queuedMsgVersion || m.ID == "" || m.Recipient == "" || m.Sender == "" || m.QueuedAt.IsZero() {
		return queuedMsg{}, errBadQueueFile
	}
	return m, nil
}

// queueFullError is the refusal of a message the recipient's queue has no
// room for. who names the pool or share that is full. quiet: the caller is
// an agent whose pool is full, told no counts, since they are other agents'.
type queueFullError struct {
	count, maxCount int
	bytes, maxBytes int64
	adding          int64
	who             string
	quiet           bool
}

func (e *queueFullError) Error() string {
	if e.quiet {
		return "its message queue is full"
	}
	return e.detail()
}

// detail is the refusal with its counts, for the audit log.
func (e *queueFullError) detail() string {
	if e.count >= e.maxCount {
		return fmt.Sprintf("%s is full (%d messages waiting, the limit is %d)", e.who, e.count, e.maxCount)
	}
	return fmt.Sprintf("%s has no room for %d more bytes (%d of %d bytes waiting)", e.who, e.adding, e.bytes, e.maxBytes)
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

// The pools of a recipient's queue, each with the full caps: the operator's
// notes, the manager's messages, and every other agent's.
const (
	poolOperator = "operator"
	poolManager  = "manager"
	poolAgents   = "agents"
)

// queuePool is the pool of a message from sender; manager is the manager's
// cert CN.
func queuePool(sender, manager string) string {
	switch sender {
	case operatorActor:
		return poolOperator
	case manager:
		return poolManager
	}
	return poolAgents
}

// add queues m behind slug's waiting messages and returns how many wait,
// m included, as m's sender may see them: the operator all of them, the
// manager all but the operator's notes, and any other agent its own only.
// manager is the manager's cert CN. A full queue refuses m before anything
// is written, and so does a ctx that ended while add waited for the lock
// (the caller's answer may be gone, and a retry would queue m twice).
//
// The queue has three pools with the same caps: the operator's notes, the
// manager's messages, and the other agents'. So no agent can fill a queue
// against the operator or the manager, and the manager's queued sends are
// never refused for what workers queued. Within the agents' pool one agent
// may hold at most half, so one agent cannot fill it against the others.
func (q *msgQueue) add(ctx context.Context, m queuedMsg, manager string) (int, error) {
	unlock := q.lock(m.Slug)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	waiting, sending, _, _, err := q.read(m.Slug)
	if err != nil {
		return 0, err
	}
	all := append(slices.Clone(waiting), sending...)
	adding := int64(len(m.Body))
	own := queuePool(m.Sender, manager)
	var pool, mine []queuedMsg
	for _, x := range all {
		if queuePool(x.Sender, manager) == own {
			pool = append(pool, x)
		}
		if x.Sender == m.Sender {
			mine = append(mine, x)
		}
	}
	who := map[string]string{poolOperator: "its queue for the operator's notes", poolManager: "its queue for the manager's messages", poolAgents: "its message queue"}[own]
	if full := (&queueFullError{count: len(pool), maxCount: q.cfg.MaxMessages, bytes: queueBytes(pool), maxBytes: q.cfg.MaxBytes, adding: adding, who: who, quiet: own == poolAgents}); full.full() {
		return 0, full
	}
	if own == poolAgents {
		share := &queueFullError{count: len(mine), maxCount: max(q.cfg.MaxMessages/2, 1), bytes: queueBytes(mine), maxBytes: max(q.cfg.MaxBytes/2, 1), adding: adding,
			who: "your share of its message queue (one sender may hold at most half of it)"}
		if share.full() {
			return 0, share
		}
	}
	dir, err := q.dir(m.Slug, true)
	if err != nil {
		return 0, err
	}
	// From the names, so a file read could not parse still holds its place.
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var next uint64 = 1
	for _, e := range ents {
		if n, ok := queueSeq(e.Name()); ok && n >= next {
			next = n + 1
		}
	}
	m.V = queuedMsgVersion
	raw, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(dir, fmt.Sprintf("%020d-%s%s", next, m.ID, queueWaiting)), raw, 0o600); err != nil {
		return 0, err
	}
	n := 1
	for _, x := range waiting {
		switch own {
		case poolOperator:
			n++
		case poolManager:
			if x.Sender != operatorActor {
				n++
			}
		default:
			if x.Sender == m.Sender {
				n++
			}
		}
	}
	return n, nil
}

// remove deletes one message's file, and forgets that it was delivered once
// its file is gone. The caller holds its recipient's lock.
func (q *msgQueue) remove(m queuedMsg) error {
	dir, err := q.dir(m.Slug, false)
	if err != nil {
		return err
	}
	err = q.removeFile(filepath.Join(dir, m.file))
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		q.mu.Lock()
		delete(q.delivered, m.ID)
		q.mu.Unlock()
	}
	return err
}

// beginSend renames m's file to .sending, marks its send under way, and
// returns m as renamed. The caller holds m's recipient's lock and ends the
// send with endSend, undoSend or dropQueued, then sendDone.
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
	q.mu.Lock()
	q.inSend[m.ID] = true
	q.mu.Unlock()
	return m, nil
}

// sendDone marks m's send no longer under way.
func (q *msgQueue) sendDone(m queuedMsg) {
	q.mu.Lock()
	delete(q.inSend, m.ID)
	q.mu.Unlock()
}

// endSend removes the file of m, which was sent. When the remove fails it
// renames the file to .sent, and when that fails too it remembers m's id,
// so what is left is dropped later without telling its sender it may not
// have arrived. The caller holds m's recipient's lock.
func (q *msgQueue) endSend(m queuedMsg) error {
	dir, err := q.dir(m.Slug, false)
	if err == nil {
		p := filepath.Join(dir, m.file)
		if err = q.removeFile(p); err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if os.Rename(p, filepath.Join(dir, strings.TrimSuffix(m.file, queueSending)+queueSent)) == nil {
			return err
		}
	}
	q.mu.Lock()
	q.delivered[m.ID] = true
	q.mu.Unlock()
	return err
}

// wasDelivered reports m sent by this broker though its file is left.
func (q *msgQueue) wasDelivered(m queuedMsg) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.delivered[m.ID]
}

// clearSent removes slug's .sent files and returns how many it removed.
// The caller holds slug's lock.
func (q *msgQueue) clearSent(slug string) int {
	dir, err := q.dir(slug, false)
	if err != nil {
		return 0
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), queueSent) && os.Remove(filepath.Join(dir, e.Name())) == nil {
			n++
		}
	}
	return n
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
	n, err := b.queue.add(ctx, m, b.manager)
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
			b.queueNotice(caller, b.senderGeneration(caller), "Lever could not wake "+slug+" to deliver the message you queued for it: "+why+". The message stays queued and goes out when "+slug+" next runs.")
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

// deliverWhenFree delivers slug's queue under its worker lock and its
// delivery guard: waiting for them when wait, else skipping a worker whose
// lock is held (a verb under way) or a delivery under way; the next pass
// tries again. The manager has no worker lock.
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
	if wait {
		b.deliverQueued(ctx, slug)
		return
	}
	if done, ok := b.queue.tryDelivery(slug); ok {
		defer done()
		b.deliverGuarded(ctx, slug)
	}
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

// dropQueued removes m and has its sender told why (queueNotice: never
// sent under the lock). The caller holds m's recipient's lock.
func (b *Broker) dropQueued(m queuedMsg, why string) {
	if err := b.queue.remove(m); err != nil && !errors.Is(err, fs.ErrNotExist) {
		b.audit("msg", m.Sender, "error", "queue->agent:"+m.Slug+": removing "+m.ID+": "+err.Error())
	}
	b.audit("msg", m.Sender, "deny", "queue->agent:"+m.Slug+": dropped: "+why, "queue_id", m.ID)
	b.queueNotice(m.Sender, m.SenderGeneration, fmt.Sprintf("Your message to %s, queued at %s, was dropped and not delivered: %s.",
		m.Slug, m.QueuedAt.UTC().Format(time.RFC3339), why))
}

// sweepLocked drops slug's interrupted sends, its expired messages and the
// ones the policy no longer allows, and returns the rest in order. A send
// under way is left to its delivery. It reads no hub and sends nothing. The
// caller holds slug's lock.
func (b *Broker) sweepLocked(slug string) []queuedMsg {
	if n := b.queue.clearSent(slug); n > 0 {
		b.audit("msg", "lever", "allow", fmt.Sprintf("queue of agent:%s: removed %d file(s) of messages already sent", slug, n))
	}
	waiting, sending, bad, skipped, err := b.queue.read(slug)
	if b.queue.readFailed(slug, err != nil) {
		// Once until it can be read again: a directory left for the
		// operator would otherwise be audited every pass.
		b.audit("msg", "lever", "error", "queue of agent:"+slug+": "+err.Error()+"; left until it can be read")
	}
	if err != nil {
		return nil
	}
	if bad > 0 {
		b.audit("msg", "lever", "error", fmt.Sprintf("queue of agent:%s: removed %d file(s) that are not queued messages", slug, bad))
	}
	if skipped > 0 {
		b.audit("msg", "lever", "error", fmt.Sprintf("queue of agent:%s: %d file(s) could not be read; left for the next pass", slug, skipped))
	}
	for _, m := range sending {
		if b.queue.sendUnderWay(m) {
			continue
		}
		if b.queue.wasDelivered(m) {
			// Sent by this broker; only its file was left.
			if err := b.queue.remove(m); err != nil && !errors.Is(err, fs.ErrNotExist) {
				b.audit("msg", m.Sender, "error", "queue->agent:"+slug+": removing "+m.ID+" after its send: "+err.Error())
			} else {
				b.audit("msg", m.Sender, "allow", "queue->agent:"+slug+": removed the file of "+m.ID+", sent earlier", "queue_id", m.ID)
			}
			continue
		}
		b.dropQueued(m, "lever was stopped while sending it, so it may or may not have arrived; it is not sent again")
	}
	now := b.now()
	var keep []queuedMsg
	for _, m := range waiting {
		if b.queue.expired(m, now) {
			b.dropQueued(m, "it waited "+b.queue.cfg.Expiry.String()+", the queue's expiry, and the recipient did not run")
			continue
		}
		if why := b.queuedStillAllowed(m); why != "" {
			b.dropQueued(m, why)
			continue
		}
		keep = append(keep, m)
	}
	return keep
}

// deliverQueued delivers slug's queue under its delivery guard, waiting for
// a delivery under way, and returns how many messages it sent. The caller
// holds slug's worker lock when slug is a worker.
func (b *Broker) deliverQueued(ctx context.Context, slug string) (sent int) {
	done, err := b.queue.waitDelivery(ctx, slug)
	if err != nil {
		return 0
	}
	defer done()
	return b.deliverGuarded(ctx, slug)
}

// deliverGuarded sends slug's queue in order while slug runs, each message
// on the live send's path, the lowest waiting first, until none waits. It
// stops at the first message it could not send, which waits in its place,
// and when ctx ends (a broker stopping) before a send begins. The queue
// lock is held only to choose a message and to change its file, never
// across the phase read or the send. The caller holds slug's delivery
// guard.
func (b *Broker) deliverGuarded(ctx context.Context, slug string) (sent int) {
	toManager := slug == b.managerSlug
	for {
		unlock := b.queue.lock(slug)
		pending := b.sweepLocked(slug)
		unlock()
		if len(pending) == 0 {
			return sent
		}
		m := pending[0]
		if b.isRevoked(m.Recipient) {
			return sent // held until it is no longer revoked, or expires
		}
		// Read before each send: the fewer sends that meet an agent gone
		// away, the fewer are dropped below.
		if _, _, phase := b.notRunningCheck(ctx, "lever", slug, toManager); phase != scion.PhaseRunning {
			return sent
		}
		// A send never begun waits in its place: only one under way when
		// the broker stops may or may not have arrived.
		if ctx.Err() != nil {
			return sent
		}
		unlock = b.queue.lock(slug)
		m, err := b.queue.beginSend(m)
		unlock()
		if errors.Is(err, fs.ErrNotExist) {
			continue // dropped by a sweep since it was chosen
		}
		if err != nil {
			b.audit("msg", m.Sender, "error", "queue->agent:"+slug+": "+err.Error())
			return sent
		}
		ref, err := b.sendRecorded(ctx, m.Recipient, slug, m.Kind, func(ref string) string { return queuedBody(m, ref) },
			scion.MsgOpts{To: "agent:" + slug, Interrupt: m.Interrupt, Project: b.instanceProject})
		if !b.endQueuedSend(m, ref, err) {
			return sent
		}
		sent++
	}
}

// endQueuedSend ends m's send, which returned ref and err, under m's
// recipient's lock, and reports it sent.
func (b *Broker) endQueuedSend(m queuedMsg, ref string, err error) bool {
	unlock := b.queue.lock(m.Slug)
	defer unlock()
	defer b.queue.sendDone(m)
	slug := m.Slug
	if errors.Is(err, errNotRecorded) {
		// Nothing left: the message waits again.
		if uerr := b.queue.undoSend(m); uerr != nil {
			b.audit("msg", m.Sender, "error", "queue->agent:"+slug+": "+uerr.Error())
		}
		return false
	}
	if err != nil {
		b.audit("msg", m.Sender, "error", "queued send->agent:"+slug+" "+ref+": "+err.Error())
		// A runtime error cannot tell a refusal from a send that arrived
		// (a timeout after the hub took it), so the message is never
		// sent again. The rest wait in order.
		b.dropQueued(m, "the send failed with a runtime error after it began, so it may or may not have arrived; it is not sent again")
		return false
	}
	if err := b.queue.endSend(m); err != nil {
		b.audit("msg", m.Sender, "error", "queue->agent:"+slug+": removing "+m.ID+" after its send: "+err.Error()+"; marked sent, it is not sent again")
	}
	b.audit("msg", m.Sender, "allow", "queued send->agent:"+slug, "kind", m.Kind, "ref", ref, "queue_id", m.ID)
	return true
}

// dropQueue drops every message waiting for slug and has their senders told
// why: its recipient is gone. It changes files only, so a destroy holding
// the worker lock never waits on a notice.
func (b *Broker) dropQueue(slug, why string) {
	if b.queue == nil || b.queue.count(slug) == 0 {
		return
	}
	unlock := b.queue.lock(slug)
	defer unlock()
	b.queue.clearSent(slug)
	waiting, sending, _, skipped, err := b.queue.read(slug)
	if err != nil {
		b.audit("msg", "lever", "error", "queue of agent:"+slug+": "+err.Error())
		return
	}
	if skipped > 0 {
		b.audit("msg", "lever", "error", fmt.Sprintf("queue of agent:%s: %d file(s) could not be read; left", slug, skipped))
	}
	for _, m := range append(sending, waiting...) {
		if b.queue.sendUnderWay(m) {
			continue // its delivery ends it
		}
		if b.queue.wasDelivered(m) {
			_ = b.queue.remove(m)
			continue
		}
		b.dropQueued(m, why)
	}
	if dir, err := b.queue.dir(slug, false); err == nil {
		_ = os.Remove(dir) // only when empty
	}
}

// queueNoticeMarker marks lever's notice to a sender about a message it
// queued (sentledger.KindQueueNotice).
const queueNoticeMarker = "[lever: message queue notice]"

// queueNoticeItem is one notice for the background sender. gen is the
// sender's generation the notice is about (senderGeneration): a notice is
// not for a later life of the name.
type queueNoticeItem struct{ sender, gen, text string }

// queueNotices is the background sender's queue of notices.
type queueNotices struct {
	mu      sync.Mutex
	pending []queueNoticeItem
	running bool
}

// queueNotice has sender told, when it is an agent that runs at the time,
// what became of a message it queued. Every notice is in the audit log; one
// to an agent goes to one background sender, so no caller waits on its
// phase read or its send, and a caller may hold any lock. With
// queueNoticesPending notices waiting, a new one is in the audit log only.
// A notice is never queued itself. gen is the generation of sender the
// notice is for; one whose sender's name has a new life since goes to the
// audit log only.
func (b *Broker) queueNotice(sender, gen, text string) {
	b.audit("msg", "lever", "allow", "queue notice for "+sender+": "+text)
	if sender == operatorActor || b.runtime == nil {
		return
	}
	n := &b.notices
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.pending) >= queueNoticesPending {
		b.audit("msg", "lever", "deny", "queue notice for "+sender+": not sent, too many notices are waiting to go out")
		return
	}
	n.pending = append(n.pending, queueNoticeItem{sender: sender, gen: gen, text: text})
	if n.running {
		return
	}
	n.running = true
	b.queueJobs.Add(1)
	go b.sendQueueNotices()
}

// sendQueueNotices sends the waiting notices one at a time, in order, and
// ends when none is left, or when the broker stops: what is left then is in
// the audit log already, and is not sent.
func (b *Broker) sendQueueNotices() {
	defer b.queueJobs.Done()
	n := &b.notices
	for {
		n.mu.Lock()
		if len(n.pending) == 0 {
			n.running = false
			n.mu.Unlock()
			return
		}
		if b.background().Err() != nil {
			left := len(n.pending)
			n.pending, n.running = nil, false
			n.mu.Unlock()
			b.audit("msg", "lever", "deny", fmt.Sprintf("%d queue notice(s) not sent: the broker is stopping", left))
			return
		}
		it := n.pending[0]
		n.pending = n.pending[1:]
		n.mu.Unlock()
		ctx, cancel := context.WithTimeout(b.background(), queueNoticeTimeout)
		b.sendQueueNotice(ctx, it)
		cancel()
	}
}

// sendQueueNotice sends one notice to its sender when that is the same life
// of an agent and runs now; otherwise the audit log has it.
func (b *Broker) sendQueueNotice(ctx context.Context, it queueNoticeItem) {
	sender, text := it.sender, it.text
	cn, slug, isManager, ok := b.identity(sender)
	if !ok || cn != sender || b.isRevoked(cn) {
		return
	}
	if b.senderGeneration(cn) != it.gen {
		b.audit("msg", "lever", "deny", "queue notice for "+sender+": not sent, the name was destroyed, purged or recycled since")
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
		if len(b.sweepLocked(slug)) > 0 {
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
// recipient's not-running refusal and why, and the same for the audit log,
// which has the counts an agent's answer leaves out. The detail of an error
// that is not a full queue stays in the audit log.
func (b *Broker) queueRefusal(caller, refusal string, err error) (answer, audited string) {
	if full := (*queueFullError)(nil); errors.As(err, &full) {
		return refusal + "; " + full.Error() + ", so nothing was queued", refusal + "; " + full.detail() + ", so nothing was queued"
	}
	b.audit("msg", caller, "error", "queue: "+err.Error())
	answer = refusal + "; the message queue could not take it (queue error), so nothing was queued"
	return answer, answer
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
	// Sending: its send began and did not end: under way, or interrupted
	// (then it is dropped).
	Sending bool   `json:"sending,omitempty"`
	Body    string `json:"body"`
}

// QueueListResponse answers PathOperatorMsgQueue. Off: there is no queue
// (the state directory is inside the tree). Unreadable names the agents
// whose queue could not be read (the audit log says why); they are left out
// of Messages.
type QueueListResponse struct {
	Off         bool         `json:"off,omitempty"`
	MaxMessages int          `json:"max_messages,omitempty"`
	MaxBytes    int64        `json:"max_bytes,omitempty"`
	Expiry      string       `json:"expiry,omitempty"`
	Messages    []QueueEntry `json:"messages"`
	Unreadable  []string     `json:"unreadable,omitempty"`
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
		waiting, sending, _, _, err := b.queue.read(slug)
		unlock()
		if err != nil {
			// One queue that cannot be read (a directory that is not
			// private is left for the operator) hides no other.
			b.audit("msg", operatorActor, "error", "queue list "+slug+": "+err.Error()+"; left out")
			out.Unreadable = append(out.Unreadable, slug)
			continue
		}
		for _, m := range append(sending, waiting...) {
			out.Messages = append(out.Messages, QueueEntry{ID: m.ID, To: m.Slug, Kind: m.Kind, QueuedAt: m.QueuedAt,
				ExpiresAt: m.QueuedAt.Add(b.queue.cfg.Expiry), Bytes: len(m.Body), Interrupt: m.Interrupt,
				Sending: strings.HasSuffix(m.file, queueSending), Body: m.Body})
		}
	}
	writeJSON(w, out)
}
