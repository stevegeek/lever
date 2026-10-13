//go:build linux || darwin

package treebackup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// countFS counts the bytes a restore writes and reports a small target
// filesystem.
type countFS struct {
	restoreFS
	written *int64
	free    int64
}

func (c countFS) write(fd int, b []byte) (int, error) {
	n, err := c.restoreFS.write(fd, b)
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
