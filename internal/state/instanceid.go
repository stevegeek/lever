package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// InstanceIDFile records this instance's identity: a random id and the state
// directory it was made for. The broker reports the id on /epoch, and every
// host-side caller of the admin port compares it with this file before
// trusting the answer: two instances on one host can share ports, version
// and broker config hash.
func (s State) InstanceIDFile() string { return filepath.Join(s.Dir, "instance-id.json") }

type instanceIDRecord struct {
	ID string `json:"id"`
	// Dir is the absolute state directory the id was made for. A copied or
	// moved instance sees another directory here (compared by identity,
	// sameDir) and makes its own id, so a copy and its original never pass
	// for one another.
	Dir string `json:"dir"`
}

const instanceIDBytes = 16

// ErrNoInstanceID means the state directory has no instance id of its own
// (absent, or made for another directory). No broker of this instance can be
// running: the broker makes the id before it binds a port.
var ErrNoInstanceID = errors.New("state: no instance id for this state directory")

// ReadInstanceID returns this state directory's instance id and never
// writes. Errors other than ErrNoInstanceID (unreadable, malformed) are
// returned as is, so a caller fails closed.
func (s State) ReadInstanceID() (string, error) {
	dir, err := filepath.Abs(s.Dir)
	if err != nil {
		return "", fmt.Errorf("state: instance id: %w", err)
	}
	rec, err := readInstanceID(s.InstanceIDFile())
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNoInstanceID
	}
	if err != nil {
		return "", err
	}
	if !sameDir(rec.Dir, dir) {
		return "", ErrNoInstanceID
	}
	return rec.ID, nil
}

// sameDir compares by identity, not spelling: a path through a symlink
// matches, a copy or a directory now at the old path does not.
func sameDir(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// EnsureInstanceID returns this state directory's instance id, making it on
// first use. Concurrent first uses agree: the file is published with a hard
// link, which fails when the name exists, and the loser reads the winner's
// id. A file made for another directory is replaced. A malformed file is an
// error, never silently replaced: the running broker may have read it.
func (s State) EnsureInstanceID() (string, error) {
	id, err := s.ReadInstanceID()
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, ErrNoInstanceID) {
		return "", err
	}
	dir, err := filepath.Abs(s.Dir)
	if err != nil {
		return "", fmt.Errorf("state: instance id: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("state: state dir: %w", err)
	}
	var raw [instanceIDBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("state: instance id: %w", err)
	}
	rec := instanceIDRecord{ID: hex.EncodeToString(raw[:]), Dir: dir}
	b, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("state: instance id: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".instance-id-*")
	if err != nil {
		return "", fmt.Errorf("state: instance id: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	_, werr := tmp.Write(append(b, '\n'))
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmpName, 0o600)
	}
	if werr != nil {
		return "", fmt.Errorf("state: write instance id: %w", werr)
	}
	path := s.InstanceIDFile()
	if s.FromAnotherDir() {
		// Made for another directory: its pid files name that directory's
		// processes, so drop them first and nothing here ever signals them.
		// Removing only a file that still names another directory keeps a
		// concurrent first use's id.
		for _, p := range []string{s.PID(), s.RemotePID()} {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", fmt.Errorf("state: drop a copied pid file: %w", err)
			}
		}
		if s.FromAnotherDir() {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", fmt.Errorf("state: replace instance id: %w", err)
			}
		}
	}
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return s.ReadInstanceID() // another process made it first
		}
		return "", fmt.Errorf("state: write instance id: %w", err)
	}
	return rec.ID, nil
}

// FromAnotherDir reports whether the instance id file was made for another
// directory (the instance was copied or moved). Its pid files then name the
// other directory's processes, so the stop paths leave them alone.
func (s State) FromAnotherDir() bool {
	dir, err := filepath.Abs(s.Dir)
	if err != nil {
		return false
	}
	rec, err := readInstanceID(s.InstanceIDFile())
	return err == nil && !sameDir(rec.Dir, dir)
}

// readInstanceID requires a 32-hex-digit id and an absolute directory.
func readInstanceID(path string) (instanceIDRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return instanceIDRecord{}, err
	}
	var rec instanceIDRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return instanceIDRecord{}, fmt.Errorf("state: %s is malformed (%v); stop this instance's broker with `lever stop`, then delete the file", filepath.Base(path), err)
	}
	if !ValidInstanceID(rec.ID) || !filepath.IsAbs(rec.Dir) {
		return instanceIDRecord{}, fmt.Errorf("state: %s is malformed; stop this instance's broker with `lever stop`, then delete the file", filepath.Base(path))
	}
	return rec, nil
}

// ValidInstanceID reports whether id is 32 lower-case hex digits.
func ValidInstanceID(id string) bool {
	if len(id) != 2*instanceIDBytes {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
