// Package sentledger is the host-side record of every message lever itself
// sends to an agent: a worker's relay, a manager message, an operator note
// (`lever msg send`) and a directive notice.
//
// Every one of those messages reaches the agent with the same envelope
// sender, the controller's hub user, because lever sends them all with the
// controller PAT. The envelope therefore cannot tell the agent who wrote the
// text, and neither can the text: anyone whose words reach an agent can type
// what lever types. This record can. The broker is the only writer: it
// writes a "send" line before it hands the body to scion, and a "done" line
// after. An agent asks the broker, over its own mTLS channel, about a
// message it received (`message_verify`); the broker answers from this
// record only, bound to the caller's own identity, and the agent acts only on
// the text the broker returns.
//
// Each send carries a fresh 128-bit id, which lever also writes on the
// message's first line as "ref=<id>". The id is only a lookup key, never
// evidence on its own: an agent that passes it gets back the text the record
// holds for that id, and only when the record names the caller as the
// recipient. A guessed or copied id finds nothing, or finds real lever text
// that was sent to the caller.
//
// Why no agent can add a line: the directory lives in the host state
// directory, outside every jail mount (the broker refuses to keep it when the
// state directory is inside the tree), and package hostledger refuses a file
// or directory anyone but the host user can write.
package sentledger

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/hostledger"
)

// The kinds of message lever sends. They are the only values a record
// carries; a line with any other kind is dropped on read.
const (
	// KindManager: the manager sent it (a note to itself included).
	KindManager = "manager"
	// KindOperatorNote: the operator sent it with `lever msg send`.
	KindOperatorNote = "operator-note"
	// KindDirectiveNotice: the broker's notice that a directive is pending.
	KindDirectiveNotice = "directive-notice"
	// KindWorkerPrefix + slug: that worker sent it. The slug is the sender's
	// identity from its certificate, never a value from the request.
	KindWorkerPrefix = "worker:"
)

// workerSlug is what a worker kind may name: a config worker name
// (config's nameRE), with room for dots and underscores.
var workerSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// WorkerKind is the kind for a message worker slug sent.
func WorkerKind(slug string) string { return KindWorkerPrefix + slug }

// ValidKind reports whether k is one of the kinds above.
func ValidKind(k string) bool {
	switch k {
	case KindManager, KindOperatorNote, KindDirectiveNotice:
		return true
	}
	slug, ok := strings.CutPrefix(k, KindWorkerPrefix)
	return ok && workerSlug.MatchString(slug)
}

// RotateCap is the size at which one (recipient, kind) file is moved to
// <file>.1 (replacing any previous .1) before the next append. Lookups read
// both files.
const RotateCap = 1 << 20

// Window is how long after lever sent a message it can still be verified.
// One-use per id (the broker's record of verified messages) already stops a
// replay, so the window only bounds how long message text stays readable on
// the host; it is long so a message the agent reads late (a long turn with
// relays queued behind it) still verifies.
const Window = 24 * time.Hour

// retention is how long a file is kept after its last append. Open removes
// older files, so the text of old messages does not pile up on the host and
// a file for a worker that no longer exists does not stay forever.
const retention = 2 * Window

// Sent is one recorded send.
type Sent struct {
	// ID is the send's id (32 hex), also written on the message's first
	// line as "ref=<ID>".
	ID string
	// Recipient is the recipient's certificate CN, the identity a verifying
	// caller presents.
	Recipient string
	// Slug is the recipient's scion agent slug.
	Slug string
	// Kind is who sent it (the Kind* constants).
	Kind string
	// Body is the exact text lever passed to scion.
	Body string
	// Before is the host clock just before the scion call; After just after
	// it (zero while the send is in flight, or when the broker stopped
	// before it could record the end).
	Before, After time.Time
	// OK is whether scion reported success. A failed send may still have
	// been delivered (a hub dispatch retry that timed out), so it stays
	// verifiable; OK is information only.
	OK bool
}

// Done reports whether the send's end was recorded.
func (s Sent) Done() bool { return !s.After.IsZero() }

// line is one JSON line in a record file: "send" before the scion call,
// "done" after it, both under the send's id.
type line struct {
	V         int        `json:"v"`
	Op        string     `json:"op"`
	ID        string     `json:"id"`
	Recipient string     `json:"recipient,omitempty"`
	Slug      string     `json:"slug,omitempty"`
	Kind      string     `json:"kind,omitempty"`
	Body      string     `json:"body,omitempty"`
	Before    *time.Time `json:"before,omitempty"`
	After     *time.Time `json:"after,omitempty"`
	OK        bool       `json:"ok,omitempty"`
}

const (
	opSend = "send"
	opDone = "done"
)

var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidID reports whether id has the form NewID makes.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// NewID mints a send id: 16 bytes from crypto/rand, in hex.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("sent ledger: id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ErrUnsafe means a record file or directory can be written by another user
// (hostledger.ErrUnsafe).
var ErrUnsafe = hostledger.ErrUnsafe

const label = "sent ledger"

// The directory holds one file per (recipient, kind), so a worker that
// floods relays to the manager can rotate out only its own file, never an
// operator note or a directive notice, and a lookup reads only the caller's
// files.
var recordFile = regexp.MustCompile(`^s-([0-9a-f]{24})-[0-9a-f]{24}\.jsonl$`)

func short(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:12])
}

// FileFor is the record file name for (recipient CN, kind).
func FileFor(recipient, kind string) string {
	return "s-" + short(recipient) + "-" + short(kind) + ".jsonl"
}

// stamp identifies a file's content as this process last saw it.
type stamp struct {
	ino  uint64
	size int64
	mod  time.Time
}

func stampOf(fi fs.FileInfo) stamp {
	return stamp{ino: hostledger.FileID(fi), size: fi.Size(), mod: fi.ModTime()}
}

// Ledger is the record directory plus an in-memory index of the sends it
// holds, so a lookup costs a map access, not a read of every file. The index
// mirrors the files: it is loaded at Open, updated by this process's own
// appends, and reloaded for a file whose content changed under it (a second
// broker appending during a restart handoff, or a rotation). Safe for
// concurrent use.
type Ledger struct {
	dir string

	mu    sync.Mutex
	files map[string]*hostledger.File // by file name
	index map[string]*Sent            // by id
	where map[string]string           // id → file name
	seen  map[string]stamp            // file name → the content the index holds
}

// Open creates dir (0700) if needed, refuses an unsafe one, removes files
// untouched for longer than twice Window, and loads the index.
func Open(dir string, now time.Time) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	if err := hostledger.CheckDir(dir, label); err != nil {
		return nil, err
	}
	l := &Ledger{dir: dir, files: map[string]*hostledger.File{}, index: map[string]*Sent{},
		where: map[string]string{}, seen: map[string]stamp{}}
	names, err := l.names("")
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		p := filepath.Join(dir, n)
		fi, err := os.Lstat(p)
		old := err == nil && now.Sub(fi.ModTime()) > retention
		if old {
			if r1, err := os.Lstat(p + ".1"); err != nil || now.Sub(r1.ModTime()) > retention {
				_ = os.Remove(p + ".1")
				_ = os.Remove(p)
				continue
			}
		}
		// A file that cannot be read safely is not fatal here: it has no
		// stamp, so every lookup for its recipient rereads it and answers
		// with the error until it is fixed (an append resets it to 0600).
		_ = l.reload(n)
	}
	return l, nil
}

// Dir is the record directory.
func (l *Ledger) Dir() string { return l.dir }

// names lists the record files in the directory whose recipient hash is
// prefix ("" for every file).
func (l *Ledger) names(recipientHash string) ([]string, error) {
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	var out []string
	for _, e := range ents {
		m := recordFile.FindStringSubmatch(e.Name())
		if m == nil || (recipientHash != "" && m[1] != recipientHash) {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

func (l *Ledger) lock() (func(), error) {
	return hostledger.LockFile(filepath.Join(l.dir, ".lock"))
}

// file returns the appender for name. Caller holds l.mu.
func (l *Ledger) file(name string) *hostledger.File {
	f := l.files[name]
	if f == nil {
		f = &hostledger.File{Path: filepath.Join(l.dir, name), Label: label, Cap: RotateCap, Lock: l.lock}
		l.files[name] = f
	}
	return f
}

// Begin records that lever is about to send e. e.ID must come from NewID,
// e.Kind must be valid and e.Before set. A send whose Begin fails must not
// happen: an unrecorded lever message could never verify.
func (l *Ledger) Begin(e Sent) error {
	switch {
	case !ValidID(e.ID):
		return fmt.Errorf("%s: bad id %q", label, e.ID)
	case !ValidKind(e.Kind):
		return fmt.Errorf("%s: bad kind %q", label, e.Kind)
	case e.Recipient == "" || e.Before.IsZero():
		return fmt.Errorf("%s: a send needs a recipient and a start time", label)
	}
	if err := hostledger.CheckDir(l.dir, label); err != nil {
		return err
	}
	before := e.Before.UTC()
	name := FileFor(e.Recipient, e.Kind)
	ln := line{V: 1, Op: opSend, ID: e.ID, Recipient: e.Recipient, Slug: e.Slug, Kind: e.Kind, Body: e.Body, Before: &before}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.file(name).Append(ln); err != nil {
		return err
	}
	e.Before, e.After, e.OK = before, time.Time{}, false
	l.put(name, &e)
	l.afterAppend(name, ln)
	return nil
}

// Done records the end of e's send.
func (l *Ledger) Done(e Sent, after time.Time, ok bool) error {
	if err := hostledger.CheckDir(l.dir, label); err != nil {
		return err
	}
	after = after.UTC()
	name := FileFor(e.Recipient, e.Kind)
	ln := line{V: 1, Op: opDone, ID: e.ID, After: &after, OK: ok}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.file(name).Append(ln); err != nil {
		return err
	}
	if s := l.index[e.ID]; s != nil && s.After.IsZero() {
		s.After, s.OK = after, ok
	}
	l.afterAppend(name, ln)
	return nil
}

// put indexes s as held by file name. Caller holds l.mu.
func (l *Ledger) put(name string, s *Sent) {
	l.index[s.ID] = s
	l.where[s.ID] = name
}

// afterAppend keeps the index in step with name after this process appended
// ln to it: when the file is exactly the one the index last saw plus that
// line, only the stamp moves; otherwise (a new file, a rotation, or another
// process's lines in between) the file is reloaded. Caller holds l.mu.
func (l *Ledger) afterAppend(name string, ln line) {
	fi, err := os.Lstat(filepath.Join(l.dir, name))
	if err != nil {
		delete(l.seen, name)
		return
	}
	raw, _ := json.Marshal(ln)
	prev, had := l.seen[name]
	now := stampOf(fi)
	if had && prev.ino == now.ino && now.size == prev.size+int64(len(raw))+1 {
		l.seen[name] = now
		return
	}
	// A reload error leaves the stamp unset, so the next lookup for this
	// recipient rereads the file and reports it.
	if err := l.reloadLocked(name); err != nil {
		delete(l.seen, name)
	}
}

// reload rereads file name (and its rotated copy) into the index.
func (l *Ledger) reload(name string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reloadLocked(name)
}

// reloadLocked replaces every index entry held by file name with what name
// and name.1 hold now. It holds the directory lock, so no appender rotates
// the file between the two reads. Caller holds l.mu.
func (l *Ledger) reloadLocked(name string) error {
	unlock, err := l.lock()
	if err != nil {
		return fmt.Errorf("%s: lock: %w", label, err)
	}
	defer unlock()
	p := filepath.Join(l.dir, name)
	byID := map[string]*Sent{}
	var order []string
	for _, f := range []string{p + ".1", p} {
		err := hostledger.ReadFile(f, label, func(raw []byte) {
			var ln line
			// A torn line (a crash mid-write), an oversized one, or one with
			// an unknown kind is skipped: every other entry is still good.
			if json.Unmarshal(raw, &ln) != nil || ln.V != 1 || !ValidID(ln.ID) {
				return
			}
			switch ln.Op {
			case opSend:
				if !ValidKind(ln.Kind) || ln.Recipient == "" || ln.Before == nil || FileFor(ln.Recipient, ln.Kind) != name {
					return
				}
				if byID[ln.ID] != nil {
					return // the first send line for an id wins
				}
				byID[ln.ID] = &Sent{ID: ln.ID, Recipient: ln.Recipient, Slug: ln.Slug, Kind: ln.Kind,
					Body: ln.Body, Before: ln.Before.UTC()}
				order = append(order, ln.ID)
			case opDone:
				if s := byID[ln.ID]; s != nil && ln.After != nil && s.After.IsZero() {
					s.After, s.OK = ln.After.UTC(), ln.OK
				}
			}
		})
		if err != nil {
			return err
		}
	}
	for id, n := range l.where {
		if n == name {
			delete(l.where, id)
			delete(l.index, id)
		}
	}
	for _, id := range order {
		if _, dup := l.index[id]; dup {
			continue // an id held by another file: keep the first
		}
		l.put(name, byID[id])
	}
	if fi, err := os.Lstat(p); err == nil {
		l.seen[name] = stampOf(fi)
	} else {
		delete(l.seen, name)
	}
	return nil
}

// refresh reloads every file of recipient whose content changed since the
// index last saw it (a handoff broker's appends). Caller holds l.mu.
func (l *Ledger) refresh(recipient string) error {
	if err := hostledger.CheckDir(l.dir, label); err != nil {
		return err
	}
	names, err := l.names(short(recipient))
	if err != nil {
		return err
	}
	for _, n := range names {
		fi, err := os.Lstat(filepath.Join(l.dir, n))
		if err != nil {
			continue
		}
		if prev, ok := l.seen[n]; ok && prev == stampOf(fi) {
			continue
		}
		if err := l.reloadLocked(n); err != nil {
			return err
		}
	}
	return nil
}

// ByRef returns the send with id whose recipient is recipient. A miss in
// the index rereads the recipient's changed files once before it answers
// false. An id recorded for another recipient is a miss.
func (l *Ledger) ByRef(recipient, id string) (Sent, bool, error) {
	if !ValidID(id) {
		return Sent{}, false, nil
	}
	if err := hostledger.CheckDir(l.dir, label); err != nil {
		return Sent{}, false, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.index[id]; s != nil && s.Recipient == recipient {
		return *s, true, nil
	}
	if err := l.refresh(recipient); err != nil {
		return Sent{}, false, err
	}
	if s := l.index[id]; s != nil && s.Recipient == recipient {
		return *s, true, nil
	}
	return Sent{}, false, nil
}

// ClockSkew is how far the envelope's clock (the jail's or the hub's) may be
// from the host clock that stamps Before and After, for InWindow.
const ClockSkew = 5 * time.Second

// MaxSendDuration bounds an in-flight send (no done line): scion's CLI call
// and the hub's dispatch retry each stop well inside it.
const MaxSendDuration = 2 * time.Minute

// Matches reports whether an envelope timestamp ts (second resolution) can
// name s, given the host clock now: from Before (truncated to the second)
// less ClockSkew to After (or, in flight, min(now, Before+MaxSendDuration))
// plus ClockSkew.
func (s Sent) Matches(ts, now time.Time) bool {
	lo := s.Before.Truncate(time.Second).Add(-ClockSkew)
	hi := s.After
	if hi.IsZero() {
		hi = s.Before.Add(MaxSendDuration)
		if now.Before(hi) {
			hi = now
		}
	}
	hi = hi.Add(ClockSkew)
	return !ts.Before(lo) && !ts.After(hi)
}

// InWindow returns the sends to recipient that an envelope timestamp ts can
// name (Matches), oldest first. It is the fallback for an agent image whose
// verify tool cannot pass a ref: a send lever made carries a ref, so a new
// image never needs it.
func (l *Ledger) InWindow(recipient string, ts, now time.Time) ([]Sent, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.refresh(recipient); err != nil {
		return nil, err
	}
	var out []Sent
	for _, s := range l.index {
		if s.Recipient == recipient && s.Matches(ts, now) {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Before.Equal(out[j].Before) {
			return out[i].Before.Before(out[j].Before)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Count is the number of record files in dir (for doctor), or an error
// when dir is unsafe. A missing directory is zero files.
func Count(dir string) (int, error) {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err := hostledger.CheckDir(dir, label); err != nil {
		return 0, err
	}
	l := &Ledger{dir: dir}
	names, err := l.names("")
	return len(names), err
}
