// Package agentledger is the host record of every message an agent was
// authorized to send to a contact (broker contact_message), and of which hub
// message each authorization was shown as.
//
// The broker is the only reader and writer. An agent sends the message itself
// (the hub stamps it as that agent); the remote proxy shows a contact an
// agent's message only when this record holds the sha256 of its exact text,
// created around the hub's time for it, and binds one record to one hub
// message, so one authorization never shows twice. The record holds hashes
// and lengths, never the text.
//
// Layout: one file per contact (c-<24 hex>.jsonl, 0600) in a 0700 directory
// in the host state directory, outside every jail mount. A lock file
// (.lock) serialises the broker and the one replacing it on a restart.
package agentledger

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/hostledger"
)

// The kinds of authorization: a message the agent starts (limited by the
// broker's initiate rule) or a reply to a verified contact post.
const (
	KindInitiated = "initiated"
	KindReply     = "reply"
	// TTL is how long after an authorization the agent has to send it.
	TTL = 10 * time.Minute
	// Skew is the clock difference allowed between the broker (host) and
	// the hub (guest VM) when a hub message time is compared with a record.
	Skew = 2 * time.Minute
	// RotateCap is the size at which a contact's file moves to .1. Bindings
	// live in the file, so a message older than two files' worth of records
	// stops showing; at ~250 bytes a line that is about 30000 messages.
	RotateCap = 4 << 20
	label     = "agent ledger"
)

// ErrUnsafe means the ledger directory or a file in it can be written by
// another user, so its records prove nothing.
var ErrUnsafe = hostledger.ErrUnsafe

// Auth is one authorization: agent may send the text whose sha256 is SHA256
// (Length characters) to contact between Created and Expires. ReplyTo is the
// hub message id of the contact post a reply answers ("" when initiated).
type Auth struct {
	ID, Agent, Contact, Kind, SHA256, ReplyTo string
	Length                                    int
	Created, Expires                          time.Time
}

// View is what an authorization is decided on: the agent's authorizations
// to this contact (all of them, oldest first) and the number of its
// authorizations to any contact in the last hour.
type View struct {
	ForContact []Auth
	LastHour   int
}

// Candidate is one agent row of a contact's history, without its text: the
// hub message id, the sha256 of its text and the hub's time for it.
type Candidate struct {
	MessageID, SHA256 string
	CreatedAt         time.Time
}

// line is one JSON line: "auth" (an authorization) or "shown" (the hub
// message an authorization was bound to, with the hash it had).
type line struct {
	V         int        `json:"v"`
	Op        string     `json:"op"`
	ID        string     `json:"id"`
	Agent     string     `json:"agent,omitempty"`
	Contact   string     `json:"contact,omitempty"`
	Kind      string     `json:"kind,omitempty"`
	SHA256    string     `json:"sha256"`
	Length    int        `json:"length,omitempty"`
	ReplyTo   string     `json:"reply_to,omitempty"`
	Created   *time.Time `json:"created,omitempty"`
	Expires   *time.Time `json:"expires,omitempty"`
	MessageID string     `json:"message_id,omitempty"`
	At        *time.Time `json:"at,omitempty"`
}

var (
	idRE      = regexp.MustCompile(`^[0-9a-f]{32}$`)
	shaRE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	messageRE = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	fileRE    = regexp.MustCompile(`^c-[0-9a-f]{24}\.jsonl$`)
)

// HashText is the hex sha256 of text's UTF-8 bytes, as records hold it.
func HashText(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

// NewID is a fresh record id (32 hex characters).
func NewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// FileFor is a contact's file name (the login is case-folded: the hub
// lowercases emails, and config refuses two logins that differ in case).
func FileFor(contact string) string {
	h := sha256.Sum256([]byte(strings.ToLower(contact)))
	return "c-" + hex.EncodeToString(h[:12]) + ".jsonl"
}

// Ledger is the agent ledger directory. Safe for concurrent use; a flock
// on .lock also serialises it with another process on the same directory.
type Ledger struct {
	dir   string
	mu    sync.Mutex
	files map[string]*hostledger.File
}

// Open creates the directory (0700) when it is missing and refuses one
// another user could write (ErrUnsafe).
func Open(dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	if err := hostledger.CheckDir(dir, label); err != nil {
		return nil, err
	}
	return &Ledger{dir: dir, files: map[string]*hostledger.File{}}, nil
}

// lock holds the in-process mutex and the directory's flock.
func (l *Ledger) lock() (func(), error) {
	l.mu.Lock()
	if err := hostledger.CheckDir(l.dir, label); err != nil {
		l.mu.Unlock()
		return nil, err
	}
	unlock, err := hostledger.LockFile(filepath.Join(l.dir, ".lock"))
	if err != nil {
		l.mu.Unlock()
		return nil, fmt.Errorf("%s: lock: %w", label, err)
	}
	return func() { unlock(); l.mu.Unlock() }, nil
}

func (l *Ledger) file(name string) *hostledger.File {
	f := l.files[name]
	if f == nil {
		f = &hostledger.File{Path: filepath.Join(l.dir, name), Label: label, Cap: RotateCap}
		l.files[name] = f
	}
	return f
}

// contactState is one contact file read back.
type contactState struct {
	auths []Auth            // oldest first
	byID  map[string]int    // auth id → index
	shown map[string]line   // hub message id → its "shown" line
	used  map[string]string // auth id → hub message id
}

func (l *Ledger) read(name string) (contactState, error) {
	s := contactState{byID: map[string]int{}, shown: map[string]line{}, used: map[string]string{}}
	p := filepath.Join(l.dir, name)
	for _, path := range []string{p + ".1", p} {
		err := hostledger.ReadFile(path, label, func(raw []byte) {
			var ln line
			if json.Unmarshal(raw, &ln) != nil || ln.V != 1 || !idRE.MatchString(ln.ID) || !shaRE.MatchString(ln.SHA256) {
				return
			}
			switch ln.Op {
			case "auth":
				if ln.Created == nil || ln.Expires == nil || (ln.Kind != KindInitiated && ln.Kind != KindReply) {
					return
				}
				if _, dup := s.byID[ln.ID]; dup {
					return
				}
				s.byID[ln.ID] = len(s.auths)
				s.auths = append(s.auths, Auth{ID: ln.ID, Agent: ln.Agent, Contact: ln.Contact, Kind: ln.Kind,
					SHA256: ln.SHA256, Length: ln.Length, ReplyTo: ln.ReplyTo, Created: *ln.Created, Expires: *ln.Expires})
			case "shown":
				if !messageRE.MatchString(ln.MessageID) {
					return
				}
				if _, taken := s.used[ln.ID]; taken {
					return // first binding wins
				}
				if _, taken := s.shown[ln.MessageID]; taken {
					return
				}
				s.used[ln.ID] = ln.MessageID
				s.shown[ln.MessageID] = ln
			}
		})
		if err != nil {
			return contactState{}, err
		}
	}
	return s, nil
}

// lastHour counts agent's authorizations to anyone since now−1h.
func (l *Ledger) lastHour(agent string, now time.Time) (int, error) {
	names, err := os.ReadDir(l.dir)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", label, err)
	}
	n := 0
	for _, e := range names {
		if !fileRE.MatchString(e.Name()) {
			continue
		}
		s, err := l.read(e.Name())
		if err != nil {
			return 0, err
		}
		for _, a := range s.auths {
			if a.Agent == agent && now.Sub(a.Created) < time.Hour {
				n++
			}
		}
	}
	return n, nil
}

func (l *Ledger) viewLocked(agent, contact string, now time.Time) (View, error) {
	s, err := l.read(FileFor(contact))
	if err != nil {
		return View{}, err
	}
	var v View
	for _, a := range s.auths {
		if a.Agent == agent && strings.EqualFold(a.Contact, contact) {
			v.ForContact = append(v.ForContact, a)
		}
	}
	v.LastHour, err = l.lastHour(agent, now)
	return v, err
}

// View reads the agent's records for contact under the lock.
func (l *Ledger) View(agent, contact string, now time.Time) (View, error) {
	unlock, err := l.lock()
	if err != nil {
		return View{}, err
	}
	defer unlock()
	return l.viewLocked(agent, contact, now)
}

// Authorize appends a when allow, called under the lock with the current
// View, returns nil; allow's error is returned unchanged and nothing is
// written. The lock makes the decision and the append one step, also across
// a broker and the one replacing it.
func (l *Ledger) Authorize(a Auth, now time.Time, allow func(View) error) error {
	if !idRE.MatchString(a.ID) || !shaRE.MatchString(a.SHA256) || (a.Kind != KindInitiated && a.Kind != KindReply) ||
		a.Agent == "" || a.Contact == "" {
		return errors.New(label + ": malformed authorization")
	}
	unlock, err := l.lock()
	if err != nil {
		return err
	}
	defer unlock()
	v, err := l.viewLocked(a.Agent, a.Contact, now)
	if err != nil {
		return err
	}
	if err := allow(v); err != nil {
		return err
	}
	c, e := a.Created.UTC(), a.Expires.UTC()
	return l.file(FileFor(a.Contact)).Append(line{V: 1, Op: "auth", ID: a.ID, Agent: a.Agent, Contact: a.Contact, Kind: a.Kind,
		SHA256: a.SHA256, Length: a.Length, ReplyTo: a.ReplyTo, Created: &c, Expires: &e})
}

// Match answers which candidates (one agent's rows of one contact's
// history) a record holds. A message already bound shows only while its
// hash is still the one it was bound with; an unbound one binds the oldest
// unused record of the same hash whose window [Created-Skew, Expires+Skew]
// holds its time, earliest message first, and the binding is appended
// before the answer. bound lists the record ids newly bound (for the audit).
// A failed append keeps none of this call's new bindings.
func (l *Ledger) Match(agent, contact string, msgs []Candidate, now time.Time) (map[string]bool, []string, error) {
	unlock, err := l.lock()
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	s, err := l.read(FileFor(contact))
	if err != nil {
		return nil, nil, err
	}
	msgs = slices.Clone(msgs)
	slices.SortFunc(msgs, func(a, b Candidate) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.MessageID, b.MessageID)
	})
	keep := map[string]bool{}
	var bound []string
	var lines []line
	for _, m := range msgs {
		if !messageRE.MatchString(m.MessageID) || !shaRE.MatchString(m.SHA256) {
			continue
		}
		if sh, ok := s.shown[m.MessageID]; ok {
			i, known := s.byID[sh.ID]
			if known && s.auths[i].Agent == agent && sh.SHA256 == m.SHA256 {
				keep[m.MessageID] = true
			}
			continue
		}
		best := -1
		for i, a := range s.auths {
			if a.Agent != agent || !strings.EqualFold(a.Contact, contact) || a.SHA256 != m.SHA256 {
				continue
			}
			if _, used := s.used[a.ID]; used {
				continue
			}
			if m.CreatedAt.Before(a.Created.Add(-Skew)) || m.CreatedAt.After(a.Expires.Add(Skew)) {
				continue
			}
			if best < 0 || a.Created.Before(s.auths[best].Created) {
				best = i
			}
		}
		if best < 0 {
			continue
		}
		a := s.auths[best]
		at := now.UTC()
		ln := line{V: 1, Op: "shown", ID: a.ID, SHA256: m.SHA256, MessageID: m.MessageID, At: &at}
		s.used[a.ID], s.shown[m.MessageID] = m.MessageID, ln
		lines = append(lines, ln)
		bound = append(bound, a.ID)
		keep[m.MessageID] = true
	}
	f := l.file(FileFor(contact))
	for _, ln := range lines {
		if err := f.Append(ln); err != nil {
			// Not recorded: show none of the new bindings, so a restart
			// cannot bind the same record to another message.
			for _, b := range lines {
				delete(keep, b.MessageID)
			}
			return keep, nil, err
		}
	}
	return keep, bound, nil
}
