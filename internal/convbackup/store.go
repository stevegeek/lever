package convbackup

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/stevegeek/lever/internal/hostledger"
)

const DirName = "conversations"

// Keep is how many backups the store keeps: Prune removes the older ones.
const Keep = 10

// Files of one backup, inside its directory.
const (
	archiveName = "projects.tar.gz"
	metaName    = "meta.json"
)

// maxMeta bounds the read of a meta.json (lever writes a few hundred bytes).
const maxMeta = 64 << 10

// idRE is the backup id: the UTC time taken, with a counter for two in one
// second. It sorts by time and is safe as a path component and on a terminal.
var idRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z(-[0-9]{1,3})?$`)

func ValidID(id string) bool { return idRE.MatchString(id) }

type Meta struct {
	ID      string    `json:"id"`
	Agent   string    `json:"agent"`
	Created time.Time `json:"created"`
	// Reason says what took the backup ("up --fresh", "manual", "before
	// restore").
	Reason  string `json:"reason"`
	Version string `json:"lever_version"`
	Stats
	// SkillHash and SessionStarted are the conversation's session record
	// (sessionrec) at backup time. A restore records them again, so the remote
	// proxy judges the restored conversation by the skill it actually read.
	SkillHash      string    `json:"skill_hash,omitempty"`
	SessionStarted time.Time `json:"session_started,omitzero"`
}

// Store is the host directory of backups: one 0700 directory per backup
// with the archive and meta.json (0600). Only lever's host user may own or
// write it: a backup holds the whole conversation and whatever secrets the
// agent saw.
type Store struct{ Dir string }

// ErrUnsafe means the store directory is not private to the host user.
var ErrUnsafe = errors.New("conversation backups: the directory is not private")

// ErrNotFound means there is no backup with that id (or none at all).
var ErrNotFound = errors.New("conversation backup not found")

// open checks the store is a real directory (not a link), owned by this user
// and closed to everyone else. create makes it (0700) when missing.
func (s Store) open(create bool) (*os.Root, error) {
	if create {
		if err := os.Mkdir(s.Dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("conversation backups: %w", err)
		}
	}
	fi, err := os.Lstat(s.Dir)
	if err != nil {
		return nil, fmt.Errorf("conversation backups: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrUnsafe, s.Dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s is %v; it must be 0700 (chmod 700 it)", ErrUnsafe, s.Dir, fi.Mode().Perm())
	}
	if owner, ok := hostledger.FileOwner(fi); ok && owner != os.Getuid() {
		return nil, fmt.Errorf("%w: %s belongs to uid %d", ErrUnsafe, s.Dir, owner)
	}
	r, err := os.OpenRoot(s.Dir)
	if err != nil {
		return nil, fmt.Errorf("conversation backups: %w", err)
	}
	return r, nil
}

func (s Store) Path(id string) string { return filepath.Join(s.Dir, id) }

// Save stores a new backup: write fills its archive, and its Stats go into m
// with the new id. It is written under a temporary name and renamed, so it
// appears whole or not at all.
func (s Store) Save(m Meta, write func(w io.Writer) (Stats, error)) (Meta, error) {
	r, err := s.open(true)
	if err != nil {
		return Meta{}, err
	}
	defer r.Close()
	if m.Created.IsZero() {
		m.Created = time.Now().UTC()
	}
	m.ID, err = freeID(r, m.Created)
	if err != nil {
		return Meta{}, err
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := ".tmp-" + m.ID + "-" + hex.EncodeToString(rnd[:])
	if err := r.Mkdir(tmp, 0o700); err != nil {
		return Meta{}, fmt.Errorf("conversation backups: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = r.RemoveAll(tmp)
		}
	}()
	f, err := r.OpenFile(filepath.Join(tmp, archiveName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Meta{}, fmt.Errorf("conversation backups: %w", err)
	}
	st, werr := write(f)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return Meta{}, werr
	}
	m.Stats = st
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Meta{}, err
	}
	if err := writeNew(r, filepath.Join(tmp, metaName), append(b, '\n')); err != nil {
		return Meta{}, err
	}
	if err := r.Rename(tmp, m.ID); err != nil {
		return Meta{}, fmt.Errorf("conversation backups: %w", err)
	}
	ok = true
	return m, nil
}

// writeNew creates name 0600, never over an existing file.
func writeNew(r *os.Root, name string, b []byte) error {
	f, err := r.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("conversation backups: %w", err)
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func freeID(r *os.Root, t time.Time) (string, error) {
	base := t.UTC().Format("20060102T150405Z")
	for i := 1; i < 1000; i++ {
		id := base
		if i > 1 {
			id += "-" + strconv.Itoa(i)
		}
		if _, err := r.Lstat(id); errors.Is(err, fs.ErrNotExist) {
			return id, nil
		}
	}
	return "", fmt.Errorf("conversation backups: no free id for %s", base)
}

// List returns every complete backup, newest first. A directory whose
// meta.json is missing or unreadable is left out.
func (s Store) List() ([]Meta, error) {
	if _, err := os.Lstat(s.Dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	r, err := s.open(false)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	d, err := r.Open(".")
	if err != nil {
		return nil, err
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		return nil, err
	}
	var out []Meta
	for _, n := range names {
		if !ValidID(n) {
			continue
		}
		if m, err := readMeta(r, n); err == nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// readMeta requires id to be a real directory holding a real archive.
func readMeta(r *os.Root, id string) (Meta, error) {
	fi, err := r.Lstat(id)
	if err != nil || !fi.IsDir() {
		return Meta{}, ErrNotFound
	}
	if fi, err := r.Lstat(filepath.Join(id, archiveName)); err != nil || !fi.Mode().IsRegular() {
		return Meta{}, ErrNotFound
	}
	f, err := r.Open(filepath.Join(id, metaName))
	if err != nil {
		return Meta{}, ErrNotFound
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return Meta{}, ErrNotFound
	}
	b, err := io.ReadAll(io.LimitReader(f, maxMeta))
	if err != nil {
		return Meta{}, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return Meta{}, fmt.Errorf("conversation backup %s: meta.json: %w", id, err)
	}
	m.ID = id
	return m, nil
}

// Get returns backup id, or the newest when id is "".
func (s Store) Get(id string) (Meta, error) {
	all, err := s.List()
	if err != nil {
		return Meta{}, err
	}
	for _, m := range all {
		if id == "" || m.ID == id {
			return m, nil
		}
	}
	if id == "" {
		return Meta{}, fmt.Errorf("%w: there are no backups in %s", ErrNotFound, s.Dir)
	}
	return Meta{}, fmt.Errorf("%w: %q (`lever conversation list` shows them)", ErrNotFound, id)
}

func (s Store) Open(id string) (io.ReadCloser, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	r, err := s.open(false)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := r.Open(filepath.Join(id, archiveName))
	if err != nil {
		return nil, fmt.Errorf("conversation backup %s: %w", id, err)
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("conversation backup %s: the archive is not a regular file", id)
	}
	return f, nil
}

// Prune removes all but the keep newest backups and returns the ids it
// removed. Only directories with a backup id's name are ever removed.
func (s Store) Prune(keep int) ([]string, error) {
	all, err := s.List()
	if err != nil || len(all) <= keep {
		return nil, err
	}
	r, err := s.open(false)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var removed []string
	for _, m := range all[keep:] {
		if !ValidID(m.ID) {
			continue
		}
		if err := r.RemoveAll(m.ID); err != nil {
			return removed, fmt.Errorf("conversation backups: removing %s: %w", m.ID, err)
		}
		removed = append(removed, m.ID)
	}
	return removed, nil
}
