//go:build linux || darwin

package treebackup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stevegeek/lever/internal/fsutil"
)

// countFS counts the bytes a restore writes and reports a small target
// filesystem.
type countFS struct {
	restoreFS
	written *int64
	free    int64
}

func (c countFS) writeAt(fd int, b []byte, off int64) (int, error) {
	n, err := c.restoreFS.writeAt(fd, b, off)
	*c.written += int64(n)
	return n, err
}

func (c countFS) freeBytes(int) (int64, error) { return c.free, nil }

// One stored file with many names is written once and the other names are
// hard links to it: a snapshot of N links to one large file must not turn
// into N copies of it on the operator's disk.
func TestRestoreHardLinksAreWrittenOnce(t *testing.T) {
	const links, size = 200, 64 << 10
	content := strings.Repeat("L", size)
	tr := newRTree().dir("d", 0o755)
	for i := 0; i < links; i++ {
		p := fmt.Sprintf("d/link%03d", i)
		tr.file(p, content, 0o644)
		e := &tr.entries[len(tr.entries)-1]
		e.Dev, e.Ino, e.Nlink = 7, 42, links
	}
	tr.file("other", content, 0o644)
	var written int64
	restoreFSWrap = func(fs restoreFS) restoreFS { return countFS{restoreFS: fs, written: &written, free: 4 * size} }
	t.Cleanup(func() { restoreFSWrap = nil })

	target := filepath.Join(t.TempDir(), "out")
	rep := restore(t, tr.opts(target))
	if !rep.OK() || rep.Files != links+1 {
		t.Fatalf("files %d, report %+v", rep.Files, rep)
	}
	if written != 2*size {
		t.Fatalf("wrote %d bytes, want %d (one copy per inode)", written, 2*size)
	}
	first, err := os.Stat(filepath.Join(target, "d/link000"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < links; i++ {
		fi, err := os.Stat(filepath.Join(target, fmt.Sprintf("d/link%03d", i)))
		if err != nil || !os.SameFile(first, fi) {
			t.Fatalf("link%03d is not a hard link of link000: %v", i, err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(target, "other")); os.SameFile(first, fi) {
		t.Fatal("a file with the same content but its own inode was linked")
	}
	if n := first.Sys().(*syscall.Stat_t).Nlink; n != links {
		t.Fatalf("nlink %d", n)
	}
	if got := readFile(t, filepath.Join(target, "d/link199")); got != content {
		t.Fatal("content")
	}
}

// A sparse file comes back sparse, and the pre-flight counts the disk its
// blob takes, not its apparent size.
func TestRestoreSparseFileStaysSparse(t *testing.T) {
	e := newSnapEnv(t)
	const size = 64 << 20
	f, err := os.Create(filepath.Join(e.tree, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(size)
	f.WriteAt([]byte("x"), size/2)
	f.Close()
	h := e.snap(t)
	s := e.store(t)
	var written int64
	restoreFSWrap = func(fs restoreFS) restoreFS { return countFS{restoreFS: fs, written: &written, free: 1 << 20} }
	t.Cleanup(func() { restoreFSWrap = nil })
	target := filepath.Join(t.TempDir(), "out")
	o := RestoreOptions{Target: target, Manifest: s.Open(h.ID), Blobs: s, InsideFD: fsutil.InsideByIdentityFD}
	rep := restore(t, o)
	if !rep.OK() || written > 64<<10 {
		t.Fatalf("wrote %d bytes; report %+v", written, rep)
	}
	fi, err := os.Stat(filepath.Join(target, "sparse"))
	if err != nil || fi.Size() != size || fi.Sys().(*syscall.Stat_t).Blocks*512 > 1<<20 {
		t.Fatalf("restored %v, %v", fi, err)
	}
}

// noLinkFS is a target that refuses hard links.
type noLinkFS struct{ restoreFS }

func (noLinkFS) linkat(int, string, int, string) error { return syscall.EPERM }

func TestRestoreHardLinksBecomeCopiesWhereLinksFail(t *testing.T) {
	tr := newRTree()
	for _, p := range []string{"a", "b"} {
		tr.file(p, "same", 0o644)
		e := &tr.entries[len(tr.entries)-1]
		e.Dev, e.Ino, e.Nlink = 7, 42, 2
	}
	restoreFSWrap = func(fs restoreFS) restoreFS { return noLinkFS{fs} }
	t.Cleanup(func() { restoreFSWrap = nil })
	target := filepath.Join(t.TempDir(), "out")
	rep := restore(t, tr.opts(target))
	if !rep.OK() || rep.Files != 2 || readFile(t, filepath.Join(target, "b")) != "same" {
		t.Fatalf("report %+v", rep)
	}
}
