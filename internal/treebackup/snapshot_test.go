//go:build linux || darwin

package treebackup

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/fsutil"
)

type snapEnv struct {
	tree, state string
}

func newSnapEnv(t *testing.T) snapEnv {
	t.Helper()
	root := t.TempDir()
	e := snapEnv{tree: filepath.Join(root, "tree"), state: filepath.Join(root, ".lever-state")}
	for _, d := range []string{e.tree, e.state} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e snapEnv) opts(trigger string) SnapshotOptions {
	l := DefaultLimits()
	l.Time = 0
	return SnapshotOptions{
		Placement:    Placement{Tree: e.tree, StateDir: e.state, Store: filepath.Join(e.state, StoreDirName)},
		Trigger:      trigger,
		LeverVersion: "test",
		Limits:       l,
		MinFree:      0,
		LockWait:     time.Second,
		Quiescent:    func(context.Context) string { return QuiescentVMOff },
	}
}

func (e snapEnv) snap(t *testing.T) *Header {
	t.Helper()
	res, err := Snapshot(context.Background(), e.opts(TriggerManual))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return res.Header
}

func (e snapEnv) store(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(e.state, StoreDirName), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// The whole path through the real store: the walker's result passes the
// writer's self-check, the readers validate it, and diff, restore, verify
// and GC work on what was committed.
func TestSnapshotEndToEnd(t *testing.T) {
	e := newSnapEnv(t)
	writeFile(t, filepath.Join(e.tree, "docs/a.md"), "alpha\nbeta\n")
	writeFile(t, filepath.Join(e.tree, "odd\xffname"), "odd\n")
	writeFile(t, filepath.Join(e.tree, "sub/x/.lever/secret"), "not a ticket\n")
	writeFile(t, filepath.Join(e.tree, ".lever/bootstrap.json"), `{"ticket":"s3cret"}`)
	if err := os.Symlink("/etc/passwd", filepath.Join(e.tree, "evil")); err != nil {
		t.Fatal(err)
	}
	h1 := e.snap(t)
	if !h1.Complete || h1.Quiescent != QuiescentVMOff || h1.Seq != 1 {
		t.Fatalf("first snapshot: complete %v quiescent %s seq %d", h1.Complete, h1.Quiescent, h1.Seq)
	}

	if err := os.Remove(filepath.Join(e.tree, "docs/a.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(e.tree, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(e.tree, "sub")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(e.tree, ".lever/bootstrap.json"), `{"ticket":"other"}`)
	h2 := e.snap(t)
	if h2.CacheBase != h1.ID || h2.KnownBase != h1.ID || h2.Seq != 2 {
		t.Fatalf("second snapshot bases: cache %q known %q seq %d", h2.CacheBase, h2.KnownBase, h2.Seq)
	}

	s := e.store(t)
	var out bytes.Buffer
	if _, err := Diff(&out, s.Open(h1.ID), s.Open(h2.ID), DiffOptions{Blobs: s}); err != nil {
		t.Fatalf("Diff: %v", err)
	}
	d := out.String()
	for _, want := range []string{"D docs/a.md", "T sub", "M .lever/bootstrap.json"} {
		if !strings.Contains(d, want) {
			t.Errorf("diff lacks %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, "s3cret") || strings.Contains(d, "other") {
		t.Errorf("diff shows the ticket's content:\n%s", d)
	}

	target := filepath.Join(t.TempDir(), "restored")
	rep, err := Restore(context.Background(), RestoreOptions{
		Target: target, Manifest: s.Open(h1.ID), Blobs: s,
		Forbidden: []string{e.tree, e.state}, InsideFD: fsutil.InsideByIdentityFD,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(target, "docs/a.md")); err != nil || string(b) != "alpha\nbeta\n" {
		t.Errorf("restored docs/a.md = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(target, "odd\xffname")); err != nil || string(b) != "odd\n" {
		t.Errorf("restored odd name = %q, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(target, "evil")); err == nil {
		t.Error("the absolute link was restored")
	}
	if _, err := os.Lstat(filepath.Join(target, ".lever/bootstrap.json")); err == nil {
		t.Error("the ticket was restored")
	}
	if rep.OK() {
		t.Error("the report must not be OK: a link and the secret were not restored")
	}

	if rep, err := s.Verify(false, time.Second); err != nil || len(rep.Problems)+len(rep.BadBlobs)+len(rep.MissingBlobs) > 0 {
		t.Fatalf("Verify: %+v, %v", rep, err)
	}
	if res, err := s.GC(GCOptions{Keep: Keep{Last: 1}, Wait: time.Second}); err != nil || res.Blocked != "" {
		t.Fatalf("GC: %+v, %v", res, err)
	}
	infos, _, err := s.List()
	if err != nil || len(infos) != 1 || infos[0].ID != h2.ID {
		t.Fatalf("after GC keep 1: %+v, %v", infos, err)
	}
}

func TestSnapshotUnreadableRootCommits(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a 0000 directory")
	}
	e := newSnapEnv(t)
	writeFile(t, filepath.Join(e.tree, "a"), "a")
	if err := os.Chmod(e.tree, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(e.tree, 0o700) })
	h := e.snap(t)
	if h.Complete || h.Root.State != StateUnreadable {
		t.Fatalf("root 0000: complete %v root %+v", h.Complete, h.Root)
	}
}

func TestSnapshotRefusesStoreInsideTree(t *testing.T) {
	e := newSnapEnv(t)
	o := e.opts(TriggerManual)
	o.StateDir = filepath.Join(e.tree, ".lever-state")
	o.Store = filepath.Join(o.StateDir, StoreDirName)
	if _, err := Snapshot(context.Background(), o); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("store inside the tree: %v", err)
	}
	if _, err := os.Lstat(o.StateDir); err == nil {
		t.Fatal("the refusal wrote into the tree")
	}

	// The same through a link: the state directory's real place is the tree.
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(e.tree, link); err != nil {
		t.Fatal(err)
	}
	o.StateDir = filepath.Join(link, "st")
	o.Store = filepath.Join(o.StateDir, StoreDirName)
	if _, err := Snapshot(context.Background(), o); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("state dir through a link into the tree: %v", err)
	}
}

func TestSnapshotRecheckDowngradesVMOff(t *testing.T) {
	e := newSnapEnv(t)
	writeFile(t, filepath.Join(e.tree, "a"), "a")
	o := e.opts(TriggerManual)
	calls := 0
	o.Quiescent = func(context.Context) string {
		calls++
		if calls == 1 {
			return QuiescentVMOff
		}
		return QuiescentNone
	}
	res, err := Snapshot(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Header.Quiescent != QuiescentNone {
		t.Fatalf("a machine that started during the walk still reads %s", res.Header.Quiescent)
	}
}

func TestSnapshotBusyIsSkippedAndRecorded(t *testing.T) {
	e := newSnapEnv(t)
	writeFile(t, filepath.Join(e.tree, "a"), "a")
	e.snap(t)
	s := e.store(t)
	l, err := s.LockSnapshot(0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	o := e.opts(TriggerStop)
	o.NoWait = true
	// A second store handle, as a second lever process would have: one
	// process never takes a lock file twice.
	if _, err := Snapshot(context.Background(), o); err == nil {
		t.Fatal("a snapshot ran while another held the snapshot lock")
	}
}

// The walker charges an upper bound of each entry line; the writer's
// self-check holds only if that bound is never below the real encoding.
func TestMaxLineBytesBoundsEncodeEntry(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	randBytes := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			switch rng.IntN(4) {
			case 0:
				b[i] = byte(rng.IntN(0x20))
			case 1:
				b[i] = byte(0x80 + rng.IntN(0x80))
			default:
				b[i] = byte(0x20 + rng.IntN(0x60))
			}
			if b[i] == '/' || b[i] == 0 {
				b[i] = 'x'
			}
		}
		return b
	}
	big := uint64(1<<64 - 1)
	for i := range 20000 {
		p := randBytes(1 + rng.IntN(300))
		e := Entry{Path: p, Type: TypeFile, HasMeta: true, Mode: 0o7777, Size: 1<<63 - 1, Mtime: -1 << 63, Ctime: -1 << 63,
			Dev: big, Ino: big, Nlink: big, SHA256: strings.Repeat("a", 64), Verified: -1 << 63,
			State: StateUnstable}
		var target []byte
		if i%2 == 0 {
			target = randBytes(rng.IntN(4096))
			e = Entry{Path: p, Type: TypeLink, HasMeta: true, Mode: 0o777, Size: 1<<63 - 1, Mtime: -1 << 63, Ctime: -1 << 63,
				Dev: big, Ino: big, Nlink: big, Target: target, HasTarget: true, State: StateListingUnstable}
		}
		if got, bound := int64(len(EncodeEntry(nil, &e))), maxLineBytes(p, target); got > bound {
			t.Fatalf("entry %q encodes to %d bytes, over the bound %d", p, got, bound)
		}
		e.Type, e.State, e.Reason, e.Target, e.HasTarget, e.SHA256, e.Verified = TypeDir, StateTruncated, ReasonDirEntries, nil, false, "", 0
		e.Listing = ListingUnreadable
		if got, bound := int64(len(EncodeEntry(nil, &e))), maxLineBytes(p, nil); got > bound {
			t.Fatalf("dir %q encodes to %d bytes, over the bound %d", p, got, bound)
		}
	}
}
