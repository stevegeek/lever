package treebackup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/fsutil"
)

// StoreDirName is the store's directory inside the state directory.
const StoreDirName = "tree-backups"

// Placement is where a store lives relative to the tree it backs up.
type Placement struct {
	Tree     string // the instance tree (operator path)
	StateDir string // .lever-state
	Store    string // <state>/tree-backups
}

// insideByIdentity is a seam over fsutil.InsideByIdentity.
var insideByIdentity = fsutil.InsideByIdentity

// CheckPlacement refuses a store an agent could reach: the store, the state
// directory or its parent inside the tree, or the tree inside the store. It
// compares file identities, so links, case aliases and firmlinks do not hide
// either case. An error from the check refuses too.
func (p Placement) CheckPlacement() error {
	for _, c := range []struct{ inner, outer, what string }{
		{p.Store, p.Tree, "the tree backup store is inside the tree"},
		{p.StateDir, p.Tree, "the state directory is inside the tree"},
		{filepath.Dir(p.StateDir), p.Tree, "the state directory's parent is inside the tree"},
		{p.Tree, p.Store, "the tree is inside the tree backup store"},
	} {
		in, err := insideByIdentity(c.inner, c.outer)
		if err != nil {
			return fmt.Errorf("%w: cannot prove %s is outside %s: %v", ErrUnsafe, c.inner, c.outer, err)
		}
		if in {
			return fmt.Errorf("%w: %s (%s, %s)", ErrUnsafe, c.what, c.inner, c.outer)
		}
	}
	return nil
}

// SnapshotOptions configure one snapshot.
type SnapshotOptions struct {
	Placement
	Trigger      string
	Pinned       bool
	LeverVersion string
	Limits       Limits
	Exclude      []string
	WorkerDirs   [][]byte
	Reverify     time.Duration
	Full         bool
	MinFree      int64
	// LockWait bounds both lock waits; NoWait makes the snapshot lock a
	// try-lock (ErrBusy when another snapshot runs).
	LockWait time.Duration
	NoWait   bool
	// Quiescent is asked before the walk and again after it: the header
	// says vm-off only when both answers do, so a racing `lever up` is not
	// recorded as a quiescent tree.
	Quiescent func(ctx context.Context) string
	Interrupt <-chan struct{}
	Now       func() time.Time
}

// SnapshotResult is a committed snapshot.
type SnapshotResult struct {
	Header   *Header
	Warnings []string
}

// Snapshot walks the tree and commits a manifest. Budgets running out make
// the snapshot incomplete, never absent; an error means nothing was
// committed. Status files record the attempt either way.
func Snapshot(ctx context.Context, o SnapshotOptions) (*SnapshotResult, error) {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	start := now().UTC()
	res, err := snapshot(ctx, o, start, now)
	if s, serr := OpenStore(o.Store, false); serr == nil {
		st := SnapshotStatus{Time: start, Trigger: o.Trigger}
		prev, _ := s.ReadSnapshotStatus()
		st.NewestID = prev.NewestID
		switch {
		case err == nil:
			st.ID = res.Header.ID
			st.NewestID = res.Header.ID
			st.Result = "complete"
			if !res.Header.Complete {
				st.Result = "incomplete"
			}
		case errors.Is(err, ErrBusy):
			st.Result = "skipped"
			st.Error = err.Error()
		default:
			st.Result = "failed"
			st.Error = err.Error()
		}
		if werr := s.WriteSnapshotStatus(st); werr != nil && res != nil {
			res.Warnings = append(res.Warnings, "writing the snapshot status: "+werr.Error())
		}
		s.Close()
	}
	return res, err
}

func snapshot(ctx context.Context, o SnapshotOptions, start time.Time, now func() time.Time) (*SnapshotResult, error) {
	if err := o.CheckPlacement(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("tree backups: %w", err)
	}
	s, err := OpenStore(o.Store, true)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	shared, err := s.LockShared(o.LockWait)
	if err != nil {
		return nil, err
	}
	defer shared.Release()
	wait := o.LockWait
	if o.NoWait {
		wait = 0
	}
	snap, err := s.LockSnapshot(wait)
	if err != nil {
		return nil, err
	}
	defer snap.Release()

	quiet := QuiescentNone
	if o.Quiescent != nil {
		quiet = o.Quiescent(ctx)
	}
	treeID, err := treeIdentity(o.Tree)
	if err != nil {
		return nil, err
	}
	cache, known, err := loadBases(s, treeID)
	if err != nil {
		return nil, err
	}
	seq, err := s.NextSeq()
	if err != nil {
		return nil, err
	}
	id, err := s.FreeID(start)
	if err != nil {
		return nil, err
	}
	wasDirty, err := s.MarkDirty()
	if err != nil {
		return nil, err
	}
	var deadline time.Time
	if o.Limits.Time > 0 {
		deadline = start.Add(o.Limits.Time)
	}
	w, err := Walk(ctx, WalkOptions{
		Tree:       o.Tree,
		Limits:     o.Limits,
		Exclude:    o.Exclude,
		WorkerDirs: o.WorkerDirs,
		Known:      known,
		Cache:      cache,
		Guard:      guardIdentities(o.Store, o.StateDir),
		Full:       o.Full,
		Reverify:   o.Reverify,
		Start:      start,
		Deadline:   deadline,
		Interrupt:  o.Interrupt,
		Sink:       s.Sink(SinkOptions{MinFree: o.MinFree, Rehash: wasDirty}),
	})
	if err != nil {
		return nil, err
	}
	if o.Quiescent != nil && quiet == QuiescentVMOff && o.Quiescent(ctx) != QuiescentVMOff {
		quiet = QuiescentNone
	}
	h := &Header{
		Version:      ManifestVersion,
		ID:           id,
		Seq:          seq,
		Created:      start,
		Finished:     now().UTC(),
		Trigger:      o.Trigger,
		Quiescent:    quiet,
		Pinned:       o.Pinned,
		LeverVersion: o.LeverVersion,
		Tree:         []byte(treeID.path),
		TreeDev:      w.TreeDev,
		TreeIno:      w.TreeIno,
		Root:         w.Root,
		Limits:       o.Limits,
		Flood:        w.Flood,
		Bytes:        w.Bytes,
		Blobs:        w.Blobs,
	}
	h.Counts.Badname = w.Badname
	h.Counts.UnreadableNew = w.UnreadableNew
	if cache != nil {
		h.CacheBase = cache.Header.ID
	}
	if known != nil {
		h.KnownBase = known.Header.ID
	}
	Summarize(h, w.Entries)
	warnings, err := s.CommitManifest(h, w.Entries)
	if err != nil {
		return nil, err
	}
	return &SnapshotResult{Header: h, Warnings: warnings}, nil
}

type treeID struct {
	path     string
	dev, ino uint64
}

func treeIdentity(tree string) (treeID, error) {
	real, err := filepath.EvalSymlinks(tree)
	if err != nil {
		return treeID{}, fmt.Errorf("tree backup: the tree: %w", err)
	}
	dev, ino, err := statIdentity(real)
	if err != nil {
		return treeID{}, fmt.Errorf("tree backup: the tree: %w", err)
	}
	return treeID{path: real, dev: dev, ino: ino}, nil
}

// guardIdentities are the directories the walker must never enter. One
// that cannot be stated (the state dir always exists by now) guards
// nothing, and CheckPlacement has already refused the aliasing cases.
func guardIdentities(paths ...string) []DevIno {
	var out []DevIno
	for _, p := range paths {
		if dev, ino, err := statIdentity(p); err == nil {
			out = append(out, DevIno{Dev: dev, Ino: ino})
		}
	}
	return out
}

// loadBases picks the cache base (the newest valid manifest of this tree)
// and the known base (the newest complete one). A manifest that fails
// validation is passed over, never trusted.
func loadBases(s *Store, id treeID) (cache, known *Base, err error) {
	infos, _, err := s.List()
	if err != nil {
		return nil, nil, err
	}
	for i := len(infos) - 1; i >= 0 && (cache == nil || known == nil); i-- {
		h := infos[i].Header
		if h == nil || h.TreeDev != id.dev || h.TreeIno != id.ino {
			continue
		}
		if cache != nil && !h.Complete {
			continue
		}
		r, err := s.Open(infos[i].ID)()
		if err != nil {
			continue
		}
		b, err := LoadBase(r)
		r.Close()
		if err != nil {
			continue
		}
		if cache == nil {
			cache = b
		}
		if known == nil && b.Header.Complete {
			known = b
		}
	}
	return cache, known, nil
}

// IsNestedLock reports whether this process runs inside `lever tree lock`,
// where the commands that write the store would wait on themselves.
func IsNestedLock() bool { return strings.TrimSpace(os.Getenv(EnvStore)) != "" }

// Environment of the command `lever tree lock` runs.
const (
	EnvStore  = "LEVER_TREE_BACKUP_STORE"
	EnvLatest = "LEVER_TREE_BACKUP_LATEST"
)
