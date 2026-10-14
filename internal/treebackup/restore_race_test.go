//go:build linux || darwin

package treebackup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/fsutil"
)

// flipFS re-points a symlink on the target's path between the restore's
// steps, as an agent racing a --to that runs through a link in the tree
// would: into the tree when the parent is opened and re-checked, out of it
// in between.
type flipFS struct {
	restoreFS
	link, in, out string
}

func (f flipFS) point(to string) {
	os.Remove(f.link)
	os.Symlink(to, f.link)
}

func (f flipFS) openParent(path string) (int, error) {
	fd, err := f.restoreFS.openParent(path)
	f.point(f.out)
	return fd, err
}

func (f flipFS) stat(path string) (fsStat, error) {
	f.point(f.in)
	return f.restoreFS.stat(path)
}

func TestRestoreTargetCheckUsesTheOpenedParent(t *testing.T) {
	base := t.TempDir()
	tree, out := filepath.Join(base, "tree"), filepath.Join(base, "out")
	os.MkdirAll(filepath.Join(tree, "inner"), 0o700)
	os.MkdirAll(out, 0o700)
	link := filepath.Join(base, "via")
	os.Symlink(filepath.Join(tree, "inner"), link)
	restoreFSWrap = func(fs restoreFS) restoreFS {
		return flipFS{restoreFS: fs, link: link, in: filepath.Join(tree, "inner"), out: out}
	}
	t.Cleanup(func() { restoreFSWrap = nil })

	o := newRTree().file("f", "x", 0o644).opts(filepath.Join(link, "restored"))
	o.Forbidden = []string{tree}
	o.InsideFD = fsutil.InsideByIdentityFD
	_, err := Restore(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "inside") {
		t.Errorf("restore through a racing link: err %v", err)
	}
	for _, p := range []string{"restored", "restored" + partialSuffix} {
		if _, err := os.Lstat(filepath.Join(tree, "inner", p)); err == nil {
			t.Errorf("the restore wrote %s into the tree", p)
		}
	}
}
