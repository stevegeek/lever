//go:build linux || darwin

package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func wantInside(t *testing.T, inner, outer string, want bool) {
	t.Helper()
	got, err := InsideByIdentity(inner, outer)
	if err != nil {
		t.Fatalf("InsideByIdentity(%s, %s): %v", inner, outer, err)
	}
	if got != want {
		t.Fatalf("InsideByIdentity(%s, %s) = %v, want %v", inner, outer, got, want)
	}
}

func TestInsideByIdentityTreeItselfIsInside(t *testing.T) {
	tree := t.TempDir()
	wantInside(t, tree, tree, true)
	wantInside(t, tree+"/", tree, true)
	wantInside(t, tree+"/.", tree, true)
}

func TestInsideByIdentityChildSiblingAndParent(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	state := filepath.Join(tree, "a", ".lever-state")
	sibling := filepath.Join(base, "tree2")
	mkdirs(t, state, sibling)
	wantInside(t, state, tree, true)
	wantInside(t, sibling, tree, false)
	wantInside(t, base, tree, false)
	wantInside(t, "/", tree, false)
	wantInside(t, state, "/", true)
}

// A state directory reached through a symlink into the tree is inside, though
// its spelling shares no prefix with the tree's.
func TestInsideByIdentitySeesThroughASymlinkedInner(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	target := filepath.Join(tree, "sub", ".lever-state")
	mkdirs(t, target)
	link := filepath.Join(base, "inst", ".lever-state")
	mkdirs(t, filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	wantInside(t, link, tree, true)
	// A relative link, and a link on an ancestor rather than the leaf.
	rel := filepath.Join(base, "rel")
	if err := os.Symlink("tree/sub", rel); err != nil {
		t.Fatal(err)
	}
	wantInside(t, filepath.Join(rel, ".lever-state"), tree, true)
}

func TestInsideByIdentitySeesThroughASymlinkedOuter(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	state := filepath.Join(tree, ".lever-state")
	mkdirs(t, state)
	link := filepath.Join(base, "tree-link")
	if err := os.Symlink(tree, link); err != nil {
		t.Fatal(err)
	}
	wantInside(t, state, link, true)
	wantInside(t, base, link, false)
}

// A ".." that follows a link is resolved by the kernel, not folded
// lexically: lexically base/link/../state is base/state, which is outside.
func TestInsideByIdentityResolvesDotDotAfterALink(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	mkdirs(t, filepath.Join(tree, "deep"), filepath.Join(tree, "state"), filepath.Join(base, "state"))
	if err := os.Symlink(filepath.Join(tree, "deep"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	wantInside(t, base+"/link/../state", tree, true)
	wantInside(t, base+"/link/../missing/x", tree, true)
}

// A path that does not exist yet is judged where it would be created.
func TestInsideByIdentityMissingInnerUsesDeepestAncestor(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	out := filepath.Join(base, "out")
	mkdirs(t, tree, out)
	wantInside(t, filepath.Join(tree, "a", "b", ".lever-state"), tree, true)
	wantInside(t, filepath.Join(out, "a", ".lever-state"), tree, false)
	// A dangling link at the leaf: the directory would be created at its
	// target, in the tree.
	dangling := filepath.Join(out, "dangling")
	if err := os.Symlink(filepath.Join(tree, "not-yet"), dangling); err != nil {
		t.Fatal(err)
	}
	wantInside(t, dangling, tree, true)
	wantInside(t, filepath.Join(dangling, "x"), tree, true)
}

func TestInsideByIdentityNonDirectoryInnerUsesItsRealParent(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	out := filepath.Join(base, "out")
	mkdirs(t, tree, out)
	file := filepath.Join(tree, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wantInside(t, file, tree, true)
	link := filepath.Join(out, "f-link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	wantInside(t, link, tree, true)
	wantInside(t, filepath.Join(file, "under-a-file"), tree, true)
	// A FIFO is judged by its directory, and opening it must not hang.
	fifo := filepath.Join(tree, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	wantInside(t, fifo, tree, true)
}

func TestInsideByIdentityMissingOuterContainsNothing(t *testing.T) {
	base := t.TempDir()
	wantInside(t, base, filepath.Join(base, "no-tree"), false)
}

// The ".." chain needs only search permission, both between inner and outer
// and above outer. A 0711 directory of another user (a home directory) is
// search-only for lever; owner modes without read (0311, 0111) prove the
// same for the test's own user.
func TestInsideByIdentitySearchOnlyAncestors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	for _, mode := range []os.FileMode{0o711, 0o311, 0o111} {
		t.Run(mode.String(), func(t *testing.T) {
			base := t.TempDir()
			above := filepath.Join(base, "above")
			tree := filepath.Join(above, "tree")
			mid := filepath.Join(tree, "mid")
			state := filepath.Join(mid, ".lever-state")
			out := filepath.Join(above, "out")
			mkdirs(t, state, out)
			for _, d := range []string{above, mid} {
				if err := os.Chmod(d, mode); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(d, 0o700) })
			}
			if _, err := os.ReadDir(above); err == nil && mode&0o400 == 0 {
				t.Fatalf("%s is still listable at %v", above, mode)
			}
			wantInside(t, state, tree, true)
			wantInside(t, filepath.Join(mid, "missing"), tree, true)
			wantInside(t, out, tree, false)
		})
	}
}

// What cannot be checked is an error, never "outside".
func TestInsideByIdentityUnsearchableAncestorIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	locked := filepath.Join(base, "locked")
	mkdirs(t, tree, filepath.Join(locked, ".lever-state"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	got, err := InsideByIdentity(filepath.Join(locked, ".lever-state"), tree)
	if err == nil {
		t.Fatalf("InsideByIdentity = %v, nil; want an error", got)
	}
	if got {
		t.Fatal("an error must come with false; the caller decides to fail closed")
	}
}

func TestInsideByIdentityLinkLoopIsAnError(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	mkdirs(t, tree)
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	if _, err := InsideByIdentity(a, tree); err == nil {
		t.Fatal("a link loop must be an error")
	}
}

// A case-insensitive filesystem (APFS) answers for any spelling of outer.
// The seam stands in for it: the state directory is found inside a tree
// named in another case, which no comparison of path strings would see.
func TestInsideByIdentitySeesACaseAlias(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	state := filepath.Join(tree, ".lever-state")
	mkdirs(t, state)
	alias := filepath.Join(base, "TREE")
	orig := statOuter
	t.Cleanup(func() { statOuter = orig })
	statOuter = func(p string, st *unix.Stat_t) error {
		if strings.EqualFold(p, tree) {
			p = tree
		}
		return unix.Stat(p, st)
	}
	if strings.HasPrefix(state, alias) {
		t.Fatal("the alias must not match lexically")
	}
	wantInside(t, state, alias, true)
	wantInside(t, filepath.Join(base, "other"), alias, false)
}

func TestParentDir(t *testing.T) {
	for in, want := range map[string]string{
		"/":       "/",
		"//":      "/",
		"/a":      "/",
		"/a/":     "/",
		"/a/b":    "/a",
		"/a/../b": "/a/..",
		"a":       ".",
		"a/b":     "a",
		".":       ".",
		"":        ".",
	} {
		if got := parentDir(in); got != want {
			t.Errorf("parentDir(%q) = %q, want %q", in, got, want)
		}
	}
}

// The FD form answers for the directory the fd holds: a path that is
// re-pointed after the open does not change the answer.
func TestInsideByIdentityFDFollowsTheDirectoryNotThePath(t *testing.T) {
	base := t.TempDir()
	tree, out := filepath.Join(base, "tree"), filepath.Join(base, "out")
	mkdirs(t, filepath.Join(tree, "sub"), out)
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(tree, "sub"), link); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(link, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(out, link); err != nil {
		t.Fatal(err)
	}
	wantInside(t, link, tree, false)
	in, err := InsideByIdentityFD(fd, link, tree)
	if err != nil || !in {
		t.Fatalf("InsideByIdentityFD = %v, %v; want inside", in, err)
	}
	ofd, err := unix.Open(out, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(ofd)
	if in, err := InsideByIdentityFD(ofd, out, tree); err != nil || in {
		t.Fatalf("InsideByIdentityFD(out) = %v, %v; want outside", in, err)
	}
}

// An agent renaming an ancestor back and forth while the check walks up
// must never make a directory inside the tree read as outside it.
func TestInsideByIdentityFDHoldsWhileAncestorsMove(t *testing.T) {
	tree := filepath.Join(t.TempDir(), "tree")
	mkdirs(t, filepath.Join(tree, "a", "b"))
	fd, err := unix.Open(filepath.Join(tree, "a", "b"), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Rename(filepath.Join(tree, "a", "b"), filepath.Join(tree, "b"))
			_ = os.Rename(filepath.Join(tree, "b"), filepath.Join(tree, "a", "b"))
		}
	}()
	outside := 0
	for i := 0; i < 5000; i++ {
		if in, err := InsideByIdentityFD(fd, "b", tree); err == nil && !in {
			outside++
		}
	}
	close(stop)
	<-done
	if outside > 0 {
		t.Fatalf("%d of 5000 checks said outside for a directory inside the tree", outside)
	}
}
