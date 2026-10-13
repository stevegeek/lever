//go:build linux

package treebackup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// validateLikeCommit runs the real writer and validator over a walk result,
// as CommitManifest's self-check does.
func validateLikeCommit(t *testing.T, res *WalkResult, lim Limits) error {
	t.Helper()
	h := &Header{Version: 1, ID: "20261013T000000Z", Seq: 1, Created: time.Now(), Finished: time.Now(),
		Trigger: TriggerManual, Quiescent: QuiescentNone, Tree: []byte("/t"), TreeDev: res.TreeDev, TreeIno: res.TreeIno,
		Root: res.Root, Limits: lim, Flood: res.Flood, Bytes: res.Bytes, Blobs: res.Blobs}
	h.Counts.Badname = res.Badname
	Summarize(h, res.Entries)
	var buf bytes.Buffer
	if err := WriteManifest(&buf, h, res.Entries); err != nil {
		t.Fatal(err)
	}
	_, err := ValidateAll(func() (EntryReader, error) {
		return NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()), h.ID)
	})
	return err
}

func TestWalkMountAfterPhaseAPassesSelfCheck(t *testing.T) {
	if !inUserns(t, "TestWalkMountAfterPhaseAPassesSelfCheck") {
		return
	}
	resetSeams(t)
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "d/f"), "x")
	o := walkOpts(tree, newMemSink())
	o.Known = baseOf(mustWalk(t, o))
	walkSeams.beforeList = func(rel []byte) {
		if len(rel) == 0 {
			if err := unix.Mount("tmpfs", filepath.Join(tree, "d"), "tmpfs", 0, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	defer unix.Unmount(filepath.Join(tree, "d"), unix.MNT_DETACH)
	res := mustWalk(t, o)
	if err := validateLikeCommit(t, res, o.Limits); err != nil {
		t.Fatalf("the writer's output fails its own validator: %v", err)
	}
}

// A name moved into a directory after its listing, out of one not listed
// yet, is in neither listing: the first directory must not stay fully
// listed.
func TestWalkMoveIntoListedDirIsListingUnstable(t *testing.T) {
	resetSeams(t)
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "a/s/x"), "")
	writeFile(t, filepath.Join(tree, "b/f"), "precious")
	old := listingWindow
	listingWindow = 20 * time.Millisecond
	defer func() { listingWindow = old }()
	time.Sleep(50 * time.Millisecond)
	moved := false
	walkSeams.beforeList = func(rel []byte) {
		if string(rel) == "a/s" && !moved {
			moved = true
			if err := os.Rename(filepath.Join(tree, "b/f"), filepath.Join(tree, "a/f")); err != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	res := mustWalk(t, walkOpts(tree, newMemSink()))
	if find(res, "a/f") != nil || find(res, "b/f") != nil {
		return
	}
	a, b := mustFind(t, res, "a"), mustFind(t, res, "b")
	if a.State == StateOK && b.State == StateOK && a.Listing == "" && b.Listing == "" {
		t.Fatalf("a file present throughout is missing from two fully listed directories:\n%s", dumpPaths(res))
	}
}

// Long hostile names, deep paths, 4 KiB control-byte targets, and limits
// lowered between snapshots: the writer's output always passes the
// validator.
func TestWalkHostileNamesAndLoweredLimitsPassSelfCheck(t *testing.T) {
	resetSeams(t)
	tree := t.TempDir()
	mk := func(comps [][]byte, fill func(fd int)) {
		fd, err := unix.Open(tree, unix.O_DIRECTORY|unix.O_RDONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range comps {
			unix.Mkdirat(fd, string(c), 0o755)
			nfd, err := unix.Openat(fd, string(c), unix.O_DIRECTORY|unix.O_RDONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			unix.Close(fd)
			fd = nfd
		}
		fill(fd)
		unix.Close(fd)
	}
	file := func(dfd int, name []byte, data string) {
		fd, err := unix.Openat(dfd, string(name), unix.O_CREAT|unix.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		unix.Write(fd, []byte(data))
		unix.Close(fd)
	}
	for _, tag := range []byte{'a', 'b', 'c', 'd'} {
		var comps [][]byte
		for range 15 {
			comps = append(comps, append(bytes.Repeat([]byte{1}, 254), tag))
		}
		mk(comps, func(fd int) {
			for i := range 30 {
				file(fd, []byte{1, 1, byte('a' + i%26), byte('a' + i/26)}, "x")
			}
			file(fd, bytes.Repeat([]byte{2}, 250), "y")
			if err := unix.Symlinkat(string(bytes.Repeat([]byte{3}, 4095)), fd, "\x01L"); err != nil {
				t.Fatal(err)
			}
		})
	}
	var comps [][]byte
	for range 15 {
		comps = append(comps, bytes.Repeat([]byte{0xff}, 255))
	}
	mk(comps, func(fd int) {
		for i := range 30 {
			file(fd, []byte{0xfe, byte('a' + i)}, "big content here")
		}
	})
	t.Cleanup(func() { os.RemoveAll(tree) })
	o := walkOpts(tree, newMemSink())
	o.Limits.DirEntries, o.Limits.Depth, o.Limits.Read = 10, 16, 20
	res := mustWalk(t, o)
	if err := validateLikeCommit(t, res, o.Limits); err != nil {
		t.Fatalf("first walk: %v", err)
	}
	o.Known = baseOf(res)
	o.Known.Header.Complete = true
	for _, lim := range []func(*Limits){
		func(l *Limits) { l.Path = 1000 },
		func(l *Limits) { l.Depth = 3 },
		func(l *Limits) { l.Manifest = 20000 },
		func(l *Limits) { l.Entries = 1; l.Manifest = 3000 },
		func(l *Limits) { l.DirEntries = 1; l.Path = 300 },
		func(l *Limits) { l.Read = 1; l.File = 2 },
	} {
		o.Limits = testLimits()
		lim(&o.Limits)
		res2, err := Walk(t.Context(), o)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateLikeCommit(t, res2, o.Limits); err != nil {
			t.Errorf("limits %+v: %v", o.Limits, err)
		}
	}
}

// An agent chmods the tree root after a first snapshot: the next one still
// commits, with the root unreadable.
func TestSnapshotUnreadableRootAfterFirstSnapshotCommits(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a 0000 directory")
	}
	e := newSnapEnv(t)
	writeFile(t, filepath.Join(e.tree, "a"), "a")
	e.snap(t)
	for _, mode := range []os.FileMode{0, 0o600, 0o300} {
		if err := os.Chmod(e.tree, mode); err != nil {
			t.Fatal(err)
		}
		res, err := Snapshot(context.Background(), e.opts(TriggerStop))
		os.Chmod(e.tree, 0o700)
		if err != nil {
			t.Fatalf("root %o: no snapshot: %v", mode, err)
		}
		if res.Header.Complete {
			t.Errorf("root %o: the snapshot reads complete", mode)
		}
	}
	if err := (Placement{Tree: e.tree, StateDir: e.state, Store: filepath.Join(e.state, StoreDirName)}).CheckPlacement(); err != nil {
		t.Fatal(err)
	}
}

// The tree as the store, or the tree inside the store, is refused.
func TestPlacementRefusesTreeInsideStore(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "st")
	store := filepath.Join(state, StoreDirName)
	if err := os.MkdirAll(filepath.Join(store, "tree"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tree := range []string{store, filepath.Join(store, "tree")} {
		err := Placement{Tree: tree, StateDir: state, Store: store}.CheckPlacement()
		if !errors.Is(err, ErrUnsafe) {
			t.Errorf("tree %s: %v", tree, err)
		}
	}
}

// A real full disk shared by the tree and the store: the snapshot commits,
// incomplete, from the reserve.
func TestSnapshotOnARealFullDiskCommits(t *testing.T) {
	if !inUserns(t, "TestSnapshotOnARealFullDiskCommits") {
		return
	}
	dir := t.TempDir()
	if err := unix.Mount("tmpfs", dir, "tmpfs", 0, "size=96m"); err != nil {
		t.Skipf("tmpfs: %v", err)
	}
	defer unix.Unmount(dir, unix.MNT_DETACH)
	e := snapEnv{tree: filepath.Join(dir, "tree"), state: filepath.Join(dir, ".lever-state")}
	os.Mkdir(e.tree, 0o700)
	os.Mkdir(e.state, 0o700)
	writeFile(t, filepath.Join(e.tree, "docs/a.md"), "alpha\n")
	e.snap(t)
	writeFile(t, filepath.Join(e.tree, "docs/a.md"), "damaged\n")
	f, _ := os.Create(filepath.Join(e.tree, "fill"))
	buf := make([]byte, 1<<20)
	for {
		if _, err := f.Write(buf); err != nil {
			break
		}
	}
	f.Close()
	for i := 0; ; i++ {
		if err := os.WriteFile(filepath.Join(e.tree, fmt.Sprintf("s%d", i)), []byte("x"), 0o600); err != nil {
			break
		}
	}
	o := e.opts(TriggerStop)
	o.MinFree = 1 << 20
	res, err := Snapshot(context.Background(), o)
	if err != nil {
		t.Fatalf("no snapshot on a full disk: %v", err)
	}
	if res.Header.Complete {
		t.Fatal("a snapshot that stored nothing new reads complete")
	}
}

// A refused placement writes nothing into the tree, not even a status.
func TestSnapshotRefusalWritesNoStatus(t *testing.T) {
	e := newSnapEnv(t)
	o := e.opts(TriggerManual)
	o.StateDir = filepath.Join(e.tree, ".lever-state")
	o.Store = filepath.Join(o.StateDir, StoreDirName)
	if err := os.MkdirAll(o.Store, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Snapshot(context.Background(), o); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("%v", err)
	}
	ents, _ := os.ReadDir(o.Store)
	if len(ents) != 0 {
		t.Fatalf("the refusal wrote into the tree: %v", ents)
	}
}

// After a crash, a garbage orphan of the right size survives snapshots that
// do not hold its content; the marker stays until GC re-hashes, and no
// later snapshot dedups onto the garbage.
func TestCrashMarkerStaysUntilGCRehashes(t *testing.T) {
	s := newStore(t)
	snap(t, s, snapOpts{files: map[string]string{"a": "x"}})
	if _, err := s.MarkDirty(); err != nil {
		t.Fatal(err)
	}
	w, _ := s.Sink(SinkOptions{}).NewBlob()
	io.WriteString(w, "hello")
	br, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	p := blobPath(s, br.Sum)
	os.Chmod(p, 0o600)
	os.WriteFile(p, []byte("HELLO"), 0o400)
	snap(t, s, snapOpts{files: map[string]string{"a": "y"}})
	if _, err := os.Lstat(filepath.Join(s.Dir(), dirtyName)); err != nil {
		t.Fatal("the marker was removed before any re-hash")
	}
	r := snap(t, s, snapOpts{files: map[string]string{"b": "hello"}})
	rep, err := s.Verify(false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(rep.BadBlobs, br.Sum) {
		t.Fatalf("snapshot %s references a garbage blob", r.id)
	}

	// A second crash orphan that no snapshot repairs: GC quarantines it.
	if _, err := s.MarkDirty(); err != nil {
		t.Fatal(err)
	}
	w, _ = s.Sink(SinkOptions{}).NewBlob()
	io.WriteString(w, "orphan")
	ob, _ := w.Commit()
	op := blobPath(s, ob.Sum)
	os.Chmod(op, 0o600)
	os.WriteFile(op, []byte("ORPHAN"), 0o400)
	snap(t, s, snapOpts{files: map[string]string{"b": "hello", "c": "orphan"}})
	res, err := s.GC(GCOptions{Keep: Keep{Last: 1}, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(s.Dir(), dirtyName)); err == nil {
		t.Fatal("GC left the marker")
	}
	if rep, _ := s.Verify(false, time.Second); len(rep.BadBlobs) > 0 {
		t.Fatalf("bad blobs after GC: %v (quarantined %v)", rep.BadBlobs, res.Quarantined)
	}
}

// A header that claims a huge file count, over a corrupt body, neither
// crashes verify nor sizes its filter.
func TestVerifyHugeHeaderCountIsBounded(t *testing.T) {
	s := newStore(t)
	snap(t, s, snapOpts{files: map[string]string{"a": "x"}})
	id := "20200101T000000Z"
	h := testHeader(id)
	h.Seq = 99
	Summarize(h, nil)
	h.Counts.ByType[TypeFile] = 9e18
	line := append(EncodeHeader(nil, h), '\n')
	if err := os.WriteFile(filepath.Join(s.Dir(), "snapshots", id+".jsonl"), line, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Verify(false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) == 0 {
		t.Fatal("the corrupt manifest was not reported")
	}
}

// A ".git" link to a gitlink file in the restore, or a git control name a
// case-insensitive target folds (Kelvin sign), is neutralised too.
func TestRestoreNeutralisesGitLinkAndFoldedNames(t *testing.T) {
	gitBin, gitErr := exec.LookPath("git")
	base := t.TempDir()
	live := filepath.Join(base, "livetree")
	marker := filepath.Join(base, "PWNED")
	tr := newRTree().dir("w", 0o755).file("w/gf", "gitdir: "+filepath.Join(live, "evil/.git")+"\n", 0o644).
		link("w/.git", "gf").file("w/x", "x", 0o644).
		dir("g", 0o755).file("g/HEAD", "ref: refs/heads/master\n", 0o644).dir("g/hooKs", 0o755).file("g/hooKs/pre-commit", "#!/bin/sh\n", 0o755)
	target := filepath.Join(base, "out")
	rep := restore(t, tr.opts(target))
	if _, err := os.Lstat(filepath.Join(target, "w/.git")); err == nil {
		t.Errorf("the .git link kept its name (neutralised %q)", rep.Neutralised)
	}
	if _, err := os.Lstat(filepath.Join(target, "g/hooKs")); err == nil {
		t.Errorf("hoo\\u212As kept its name (neutralised %q)", rep.Neutralised)
	}
	if gitErr != nil {
		return
	}
	if out, err := exec.Command(gitBin, "init", "-q", filepath.Join(live, "evil")).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	exec.Command(gitBin, "-C", filepath.Join(live, "evil"), "config", "core.fsmonitor", "touch "+marker+"; false").Run()
	home := filepath.Join(base, "home")
	os.Mkdir(home, 0o700)
	cmd := exec.Command(gitBin, "status", "--porcelain")
	cmd.Dir = filepath.Join(target, "w")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CEILING_DIRECTORIES=" + base}
	cmd.CombinedOutput()
	if _, err := os.Lstat(marker); err == nil {
		t.Error("git ran the planted command through the .git link")
	}
}
