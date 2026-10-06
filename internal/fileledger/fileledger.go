// Package fileledger is the host record of the chat page's file exchange
// (remote.files): every upload the remote proxy stored for a login and
// every file an agent shared with one (broker share_file), with the sha256
// and size lever saw. A file in an agent's workspace means nothing without
// a record here: an agent trusts an upload only as listed, and a download
// serves only bytes that still hash to their record.
//
// Two processes write it — the proxy (uploads) and the broker (shares) —
// so every append runs under a mutex and a flock on .lock, and reads the
// agent's records first for the limits. Layout: one file per agent
// (<agent>.jsonl, 0600) in a 0700 directory in the state directory, outside
// every jail mount. Records hold names, hashes and sizes, never content.
package fileledger

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/chatfiles"
	"github.com/stevegeek/lever/internal/hostledger"
)

const (
	OpUpload = "upload"
	OpShare  = "share"
	// RotateCap: past it an agent's file moves to .1 on the next append. A
	// second move drops the first file's records, and with them the right
	// to download those files; at ~400 bytes a record that is about 20000
	// records an agent.
	RotateCap = 8 << 20
	label     = "files ledger"
)

// ErrUnsafe: a ledger file or directory another user can write.
var ErrUnsafe = hostledger.ErrUnsafe

// Record is one upload (proxy) or share (broker). Rel is tree-relative.
type Record struct {
	V      int       `json:"v"`
	Op     string    `json:"op"`
	ID     string    `json:"id"`
	Agent  string    `json:"agent"`
	Login  string    `json:"login"`
	Name   string    `json:"name"`
	Rel    string    `json:"rel"`
	SHA256 string    `json:"sha256"`
	Size   int64     `json:"size"`
	At     time.Time `json:"at"`
}

var (
	agentRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	idRE    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	shaRE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func (r Record) valid() bool {
	return r.V == 1 && (r.Op == OpUpload || r.Op == OpShare) && idRE.MatchString(r.ID) && agentRE.MatchString(r.Agent) &&
		r.Login != "" && len(r.Login) <= 320 && r.Name != "" && len(r.Name) <= chatfiles.MaxNameLen &&
		r.Rel != "" && path.Clean(r.Rel) == r.Rel && filepath.IsLocal(filepath.FromSlash(r.Rel)) &&
		shaRE.MatchString(r.SHA256) && r.Size >= 0 && !r.At.IsZero()
}

// IsAgentFile reports whether name is an agent's ledger file (main or .1).
func IsAgentFile(name string) bool {
	n, ok := strings.CutSuffix(strings.TrimSuffix(name, ".1"), ".jsonl")
	return ok && agentRE.MatchString(n)
}

// NewID is a fresh record id: 32 hex digits from crypto/rand.
func NewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Ledger is the files ledger directory. Safe for concurrent use, and for
// several processes on one directory (flock).
type Ledger struct {
	dir   string
	cap   int64 // RotateCap; tests shrink it
	mu    sync.Mutex
	files map[string]*hostledger.File
}

// Open creates (0700) or checks the ledger directory dir.
func Open(dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	if err := hostledger.CheckDir(dir, label); err != nil {
		return nil, err
	}
	// MkdirAll keeps an existing directory's mode: set it, so no other user
	// even lists the agents' files.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return &Ledger{dir: dir, cap: RotateCap, files: map[string]*hostledger.File{}}, nil
}

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

// read is agent's records, oldest first: the rotated file, then the main
// one. A line that does not decode, is not valid, or names another agent is
// skipped (a torn last line after a crash).
func (l *Ledger) read(agent string) ([]Record, error) {
	if !agentRE.MatchString(agent) {
		return nil, fmt.Errorf("%s: bad agent name %q", label, agent)
	}
	var out []Record
	p := filepath.Join(l.dir, agent+".jsonl")
	for _, f := range []string{p + ".1", p} {
		err := hostledger.ReadFile(f, label, func(line []byte) {
			var r Record
			dec := json.NewDecoder(bytes.NewReader(line))
			if dec.Decode(&r) == nil && r.valid() && r.Agent == agent {
				out = append(out, r)
			}
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Add validates r and appends it to its agent's file. Under the mutex and
// the flock it first reads the agent's records and calls allow (nil: no
// limit); allow's error is returned unchanged and nothing is written, so a
// rate or quota check and the append are one step for both writers.
func (l *Ledger) Add(r Record, allow func(prior []Record) error) error {
	if !r.valid() {
		return fmt.Errorf("%s: refusing an invalid record", label)
	}
	return l.AddWith(r.Agent, func(prior []Record) (Record, error) {
		if allow != nil {
			if err := allow(prior); err != nil {
				return Record{}, err
			}
		}
		return r, nil
	})
}

// AddWith is Add for a record that exists only once a step has run under
// the lock: build reads agent's records, may refuse (its error is returned
// unchanged, nothing is written), and returns the record to append. The
// remote proxy checks an upload's limits and only then writes the file
// into the tree, all under the lock, so a refused upload never touches the
// tree. When the append fails after build wrote something, the caller
// removes it.
func (l *Ledger) AddWith(agent string, build func(prior []Record) (Record, error)) error {
	unlock, err := l.lock()
	if err != nil {
		return err
	}
	defer unlock()
	prior, err := l.read(agent)
	if err != nil {
		return err
	}
	r, err := build(prior)
	if err != nil {
		return err
	}
	if !r.valid() || r.Agent != agent {
		return fmt.Errorf("%s: refusing an invalid record", label)
	}
	f := l.files[r.Agent]
	if f == nil {
		f = &hostledger.File{Path: filepath.Join(l.dir, r.Agent+".jsonl"), Label: label, Cap: l.cap}
		l.files[r.Agent] = f
	}
	return f.Append(r)
}

// List is the agent's records, oldest first.
func (l *Ledger) List(agent string) ([]Record, error) {
	unlock, err := l.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return l.read(agent)
}

// Find is the agent's record id; another agent's id is not found.
func (l *Ledger) Find(agent, id string) (Record, bool, error) {
	recs, err := l.List(agent)
	if err != nil {
		return Record{}, false, err
	}
	for _, r := range recs {
		if r.ID == id {
			return r, true, nil
		}
	}
	return Record{}, false, nil
}
