package fsutil

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestWriteFileAtomicHonoursPerm(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := WriteFileAtomic(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v err = %v, want 0644", fi.Mode().Perm(), err)
	}
}

func TestWriteFileAtomicReplacesContentAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(p, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Fatalf("content = %q, want new", b)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want only the target", len(entries))
	}
}

// --- tree-confined reads and writes ---

// treeFixture returns an instance root with a tree beneath it and a host file
// OUTSIDE the tree that no tree operation may touch.
func treeFixture(t *testing.T) (tree, hostFile string) {
	t.Helper()
	root := t.TempDir()
	tree = filepath.Join(root, "workspace")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	hostFile = filepath.Join(root, "lever.yaml")
	if err := os.WriteFile(hostFile, []byte("host: config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return tree, hostFile
}

func assertHostUntouched(t *testing.T, hostFile string) {
	t.Helper()
	if b, err := os.ReadFile(hostFile); err != nil || string(b) != "host: config\n" {
		t.Fatalf("host file changed: %q err=%v", b, err)
	}
}

func TestReadInTreeAbsentIsNotExist(t *testing.T) {
	tree, _ := treeFixture(t)
	_, err := ReadInTree(tree, "CLAUDE.md")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent file: err=%v, want fs.ErrNotExist", err)
	}
}

func TestWriteInTreeCreatesParentsAndReadsBack(t *testing.T) {
	tree, _ := treeFixture(t)
	rel := ".claude/skills/lever-operator/SKILL.md"
	if err := WriteInTree(tree, rel, []byte("scaffold"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := ReadInTree(tree, rel)
	if err != nil || string(b) != "scaffold" {
		t.Fatalf("read back: %q err=%v", b, err)
	}
	fi, _ := os.Stat(filepath.Join(tree, filepath.FromSlash(rel)))
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", fi.Mode().Perm())
	}
}

func TestWriteInTreeKeepsExistingModeAndInode(t *testing.T) {
	tree, _ := treeFixture(t)
	p := filepath.Join(tree, "CLAUDE.md")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteInTree(tree, "CLAUDE.md", []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("in-place write must keep the file's mode: %v", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Fatalf("content = %q", b)
	}
}

// The case commit 22880a9 protected: a CLAUDE.md that is itself a symlink to
// another file INSIDE the tree is written through, in place.
func TestWriteInTreeFollowsSymlinkInsideTree(t *testing.T) {
	tree, _ := treeFixture(t)
	real := filepath.Join(tree, "docs", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tree, "CLAUDE.md")
	if err := os.Symlink(filepath.Join("docs", "CLAUDE.md"), link); err != nil {
		t.Fatal(err)
	}
	if err := WriteInTree(tree, "CLAUDE.md", []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&fs.ModeSymlink == 0 {
		t.Fatal("the link must survive the write")
	}
	if b, _ := os.ReadFile(real); string(b) != "new" {
		t.Fatalf("link target not written: %q", b)
	}
	if b, err := ReadInTree(tree, "CLAUDE.md"); err != nil || string(b) != "new" {
		t.Fatalf("read through in-tree link: %q err=%v", b, err)
	}
}

// A symlinked DIRECTORY inside the tree is fine too (.claude -> shared/.claude).
func TestWriteInTreeFollowsDirSymlinkInsideTree(t *testing.T) {
	tree, _ := treeFixture(t)
	if err := os.MkdirAll(filepath.Join(tree, "shared", ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("shared", ".claude"), filepath.Join(tree, ".claude")); err != nil {
		t.Fatal(err)
	}
	rel := ".claude/skills/lever-operator/SKILL.md"
	if err := WriteInTree(tree, rel, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(tree, "shared", ".claude", "skills", "lever-operator", "SKILL.md")); err != nil || string(b) != "s" {
		t.Fatalf("not written through the dir link: %q err=%v", b, err)
	}
}

func TestTreeRefusesSymlinkToHostFile(t *testing.T) {
	tree, hostFile := treeFixture(t)
	cases := []struct {
		name, rel, target string
	}{
		{"relative leaf link", "CLAUDE.md", filepath.Join("..", "lever.yaml")},
		{"absolute leaf link", "CLAUDE.md", hostFile},
		{"dangling relative leaf link", "CLAUDE.md", filepath.Join("..", "does-not-exist")},
		{"dangling absolute leaf link", "CLAUDE.md", filepath.Join(filepath.Dir(hostFile), "nope")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			link := filepath.Join(tree, c.rel)
			if err := os.Symlink(c.target, link); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(link)
			if _, err := ReadInTree(tree, c.rel); !errors.Is(err, ErrEscapesTree) {
				t.Fatalf("read: err=%v, want ErrEscapesTree", err)
			}
			if err := WriteInTree(tree, c.rel, []byte("x"), 0o644); !errors.Is(err, ErrEscapesTree) {
				t.Fatalf("write: err=%v, want ErrEscapesTree", err)
			}
			assertHostUntouched(t, hostFile)
			if _, err := os.Stat(filepath.Join(filepath.Dir(hostFile), "does-not-exist")); err == nil {
				t.Fatal("dangling link target must not be created")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(hostFile), "nope")); err == nil {
				t.Fatal("dangling link target must not be created")
			}
		})
	}
}

// A directory component (.claude) replaced by a link out of the tree is
// refused before any mkdir or write, both on the create path (leaf absent)
// and when the target already holds a file.
func TestTreeRefusesDirSymlinkOutOfTree(t *testing.T) {
	tree, hostFile := treeFixture(t)
	outside := filepath.Join(filepath.Dir(hostFile), "elsewhere")
	if err := os.MkdirAll(filepath.Join(outside, "skills", "lever-operator"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, ".claude")); err != nil {
		t.Fatal(err)
	}
	rel := ".claude/skills/lever-operator/SKILL.md"
	if _, err := ReadInTree(tree, rel); !errors.Is(err, ErrEscapesTree) {
		t.Fatalf("read: err=%v, want ErrEscapesTree", err)
	}
	if err := WriteInTree(tree, rel, []byte("x"), 0o644); !errors.Is(err, ErrEscapesTree) {
		t.Fatalf("write: err=%v, want ErrEscapesTree", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "skills", "lever-operator", "SKILL.md")); err == nil {
		t.Fatal("wrote through the directory link")
	}
	// A dangling directory link is refused the same way (never created through).
	if err := os.Remove(filepath.Join(tree, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "gone"), filepath.Join(tree, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := WriteInTree(tree, rel, []byte("x"), 0o644); !errors.Is(err, ErrEscapesTree) {
		t.Fatalf("dangling dir link write: err=%v, want ErrEscapesTree", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(hostFile), "gone")); err == nil {
		t.Fatal("dangling dir link target must not be created")
	}
}

func TestTreeRefusesNonRegularLeaf(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs on windows")
	}
	tree, _ := treeFixture(t)
	fifo := filepath.Join(tree, "CLAUDE.md")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() {
		_, err := ReadInTree(tree, "CLAUDE.md")
		done <- err
	}()
	go func() { done <- WriteInTree(tree, "CLAUDE.md", []byte("x"), 0o644) }()
	for range 2 {
		select {
		case err := <-done:
			if !errors.Is(err, ErrNotRegularFile) {
				t.Fatalf("FIFO: err=%v, want ErrNotRegularFile", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("FIFO blocked the operation (the hang the guard exists to prevent)")
		}
	}
	// A directory where a file is expected is not regular either.
	if err := os.Mkdir(filepath.Join(tree, "dir.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInTree(tree, "dir.md"); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("dir: err=%v, want ErrNotRegularFile", err)
	}
}

func TestReadInTreeCapsSize(t *testing.T) {
	tree, _ := treeFixture(t)
	big := make([]byte, MaxTreeFileSize+1)
	if err := os.WriteFile(filepath.Join(tree, "CLAUDE.md"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInTree(tree, "CLAUDE.md"); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("oversize: err=%v, want ErrFileTooLarge", err)
	}
	if err := os.WriteFile(filepath.Join(tree, "CLAUDE.md"), big[:MaxTreeFileSize], 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadInTree(tree, "CLAUDE.md"); err != nil || len(b) != MaxTreeFileSize {
		t.Fatalf("at the cap: len=%d err=%v", len(b), err)
	}
}

func TestTreeRefusesNonLocalRel(t *testing.T) {
	tree, hostFile := treeFixture(t)
	for _, rel := range []string{"../lever.yaml", hostFile, "a/../../lever.yaml"} {
		if _, err := ReadInTree(tree, rel); !errors.Is(err, ErrEscapesTree) {
			t.Errorf("read %q: err=%v, want ErrEscapesTree", rel, err)
		}
		if err := WriteInTree(tree, rel, []byte("x"), 0o644); !errors.Is(err, ErrEscapesTree) {
			t.Errorf("write %q: err=%v, want ErrEscapesTree", rel, err)
		}
	}
	assertHostUntouched(t, hostFile)
}

func TestRelOverlapFold(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"a/b", "a/b", true},
		{"a/b", "A/B", true},
		{"a", "A/b/c", true},
		{"a/b/c", "a", true},
		{"./a/b/", "a/b", true},
		{".", "x", true},
		{"a/b", "a/bc", false},
		{"a/b", "a/c", false},
		{"workers/w", "assistant/tools", false},
	} {
		if got := RelOverlapFold(tc.a, tc.b); got != tc.want {
			t.Errorf("RelOverlapFold(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestReadInTreeNoLinks(t *testing.T) {
	tree := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(tree, "workers", "real"), 0o755))
	must(t, os.WriteFile(filepath.Join(tree, "workers", "labels.json"), []byte(`{}`), 0o644))
	if b, err := ReadInTreeNoLinks(tree, "workers/labels.json", 16); err != nil || string(b) != "{}" {
		t.Fatalf("plain file: %q %v", b, err)
	}
	// A link on any component is refused, even one that stays in the tree.
	must(t, os.Symlink("real", filepath.Join(tree, "workers", "link")))
	must(t, os.WriteFile(filepath.Join(tree, "workers", "real", "l.json"), []byte(`{}`), 0o644))
	if _, err := ReadInTreeNoLinks(tree, "workers/link/l.json", 16); !errors.Is(err, ErrSymlink) {
		t.Fatalf("dir link: %v, want ErrSymlink", err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	must(t, os.WriteFile(outside, []byte("x"), 0o644))
	must(t, os.Symlink(outside, filepath.Join(tree, "workers", "leaf.json")))
	if _, err := ReadInTreeNoLinks(tree, "workers/leaf.json", 16); !errors.Is(err, ErrSymlink) {
		t.Fatalf("leaf link: %v, want ErrSymlink", err)
	}
	must(t, os.WriteFile(filepath.Join(tree, "big.json"), bytes.Repeat([]byte("x"), 17), 0o644))
	if _, err := ReadInTreeNoLinks(tree, "big.json", 16); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("big: %v", err)
	}
	if _, err := ReadInTreeNoLinks(tree, "workers", 16); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("dir: %v", err)
	}
	if _, err := ReadInTreeNoLinks(tree, "nope.json", 16); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent: %v", err)
	}
	if _, err := ReadInTreeNoLinks(tree, "../x", 16); !errors.Is(err, ErrEscapesTree) {
		t.Fatalf("dotdot: %v", err)
	}
	if _, err := ReadInTreeNoLinks(tree, "/etc/hosts", 16); !errors.Is(err, ErrEscapesTree) {
		t.Fatalf("absolute: %v", err)
	}
	// A FIFO is not a regular file and never blocks the read.
	must(t, syscall.Mkfifo(filepath.Join(tree, "fifo"), 0o644))
	if _, err := ReadInTreeNoLinks(tree, "fifo", 16); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("fifo: %v", err)
	}
}

// A leaf swapped between the Lstat walk and the open is refused: the open
// must land on the file the walk saw. Each swap here passes the walk and
// the os.Root open, so only the post-open check stands in its way.
func TestReadInTreeNoLinksRefusesASwapAfterTheWalk(t *testing.T) {
	for name, swap := range map[string]func(t *testing.T, tree, leaf string){
		"another regular file": func(t *testing.T, tree, leaf string) {
			other := filepath.Join(tree, "other.json")
			must(t, os.WriteFile(other, []byte(`{"x":"swapped"}`), 0o644))
			must(t, os.Rename(other, leaf))
		},
		"an in-tree symlink": func(t *testing.T, tree, leaf string) {
			must(t, os.WriteFile(filepath.Join(tree, "secret.json"), []byte(`{"x":"secret"}`), 0o644))
			must(t, os.Remove(leaf))
			must(t, os.Symlink("secret.json", leaf))
		},
		"a FIFO": func(t *testing.T, tree, leaf string) {
			must(t, os.Remove(leaf))
			must(t, syscall.Mkfifo(leaf, 0o644))
		},
	} {
		t.Run(name, func(t *testing.T) {
			tree := t.TempDir()
			leaf := filepath.Join(tree, "labels.json")
			must(t, os.WriteFile(leaf, []byte(`{}`), 0o644))
			afterNoLinkWalk = func() { swap(t, tree, leaf) }
			t.Cleanup(func() { afterNoLinkWalk = nil })
			b, err := ReadInTreeNoLinks(tree, "labels.json", 1024)
			if !errors.Is(err, ErrSymlink) {
				t.Fatalf("read %q, %v; want ErrSymlink", b, err)
			}
		})
	}
}
func TestCreateInTreeNoLinksCreatesParentsAndFile(t *testing.T) {
	tree := t.TempDir()
	f, err := CreateInTreeNoLinks(tree, "a/b/c.txt", 0o755, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("hello"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	fi, err := os.Lstat(filepath.Join(tree, "a/b/c.txt"))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o644 {
		t.Fatalf("leaf = %v, %v", fi, err)
	}
	if b, _ := os.ReadFile(filepath.Join(tree, "a/b/c.txt")); string(b) != "hello" {
		t.Fatalf("content %q", b)
	}
	if fi, err := os.Lstat(filepath.Join(tree, "a/b")); err != nil || !fi.IsDir() {
		t.Fatalf("parent = %v, %v", fi, err)
	}
}

func TestCreateInTreeNoLinksRefusesExisting(t *testing.T) {
	tree := t.TempDir()
	f, err := CreateInTreeNoLinks(tree, "x.txt", 0o755, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := CreateInTreeNoLinks(tree, "x.txt", 0o755, 0o644); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second create = %v, want ErrExist", err)
	}
}

func TestCreateInTreeNoLinksRefusesLinkLeaf(t *testing.T) {
	tree, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "victim")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(tree, "x.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateInTreeNoLinks(tree, "x.txt", 0o755, 0o644); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("create over a link = %v, want ErrExist", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("link target changed: %q", b)
	}
}

func TestCreateInTreeNoLinksRefusesLinkOnPath(t *testing.T) {
	for name, link := range map[string]func(tree, outside string) string{
		"out of tree": func(tree, outside string) string { return outside },
		"in tree":     func(tree, outside string) string { return filepath.Join(tree, "real") },
	} {
		t.Run(name, func(t *testing.T) {
			tree, outside := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Join(tree, "real"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(tree, "a"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(link(tree, outside), filepath.Join(tree, "a", "b")); err != nil {
				t.Fatal(err)
			}
			if _, err := CreateInTreeNoLinks(tree, "a/b/c.txt", 0o755, 0o644); !errors.Is(err, ErrSymlink) {
				t.Fatalf("err = %v, want ErrSymlink", err)
			}
			for _, d := range []string{outside, filepath.Join(tree, "real")} {
				if ents, _ := os.ReadDir(d); len(ents) != 0 {
					t.Fatalf("%s got %v", d, ents)
				}
			}
		})
	}
}

// The "sibling" link is relative and stays inside the parent's Root, so
// os.Root opens it: only the SameFile check refuses it. The absolute one
// leaves the Root and os.Root refuses it itself.
func TestCreateInTreeNoLinksRefusesSwapAfterCheck(t *testing.T) {
	for name, link := range map[string]func(tree, outside string) string{
		"out of tree": func(tree, outside string) string { return outside },
		"sibling":     func(tree, outside string) string { return "real" },
	} {
		t.Run(name, func(t *testing.T) {
			tree, outside := t.TempDir(), t.TempDir()
			for _, d := range []string{"a/real", "a/b"} {
				if err := os.MkdirAll(filepath.Join(tree, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			afterNoLinkStep = func(n string) {
				if n != "b" {
					return
				}
				b := filepath.Join(tree, "a", "b")
				_ = os.Rename(b, b+".moved")
				_ = os.Symlink(link(tree, outside), b)
			}
			defer func() { afterNoLinkStep = nil }()
			if _, err := CreateInTreeNoLinks(tree, "a/b/c.txt", 0o755, 0o644); !errors.Is(err, ErrSymlink) {
				t.Fatalf("a component swapped for a link after its check: %v, want ErrSymlink", err)
			}
			for _, d := range []string{outside, filepath.Join(tree, "a", "real")} {
				if ents, _ := os.ReadDir(d); len(ents) != 0 {
					t.Fatalf("%s got %v", d, ents)
				}
			}
		})
	}
}

func TestCreateInTreeNoLinksRefusesEscape(t *testing.T) {
	tree := t.TempDir()
	for _, rel := range []string{"", ".", "../x", "/abs/x", "a/../../x", "a//b", "a/./b"} {
		if _, err := CreateInTreeNoLinks(tree, rel, 0o755, 0o644); !errors.Is(err, ErrEscapesTree) {
			t.Errorf("%q: err = %v, want ErrEscapesTree", rel, err)
		}
	}
}

func TestCreateInTreeNoLinksTreeItselfMayBeALink(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "tree")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	f, err := CreateInTreeNoLinks(link, "a/x.txt", 0o755, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := os.Stat(filepath.Join(real, "a", "x.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestOpenInTreeNoLinks(t *testing.T) {
	tree, outside := t.TempDir(), t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(tree, "d"), 0o755))
	must(os.WriteFile(filepath.Join(tree, "d", "ok.txt"), []byte("12345"), 0o644))
	must(os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o600))
	must(os.Symlink(filepath.Join(outside, "secret"), filepath.Join(tree, "d", "link.txt")))
	must(os.Symlink(filepath.Join(tree, "d"), filepath.Join(tree, "dl")))
	must(syscall.Mkfifo(filepath.Join(tree, "d", "fifo"), 0o644))

	f, fi, err := OpenInTreeNoLinks(tree, "d/ok.txt", 10)
	if err != nil || fi.Size() != 5 {
		t.Fatalf("ok = %v %v", fi, err)
	}
	f.Close()
	for rel, want := range map[string]error{
		"d/link.txt": ErrSymlink,
		"dl/ok.txt":  ErrSymlink,
		"d/fifo":     ErrNotRegularFile,
		"d/none":     fs.ErrNotExist,
		"../x":       ErrEscapesTree,
	} {
		if _, _, err := OpenInTreeNoLinks(tree, rel, 10); !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", rel, err, want)
		}
	}
	if _, _, err := OpenInTreeNoLinks(tree, "d/ok.txt", 4); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("max: %v", err)
	}
	afterNoLinkWalk = func() {
		_ = os.Remove(filepath.Join(tree, "d", "ok.txt"))
		_ = os.Symlink(filepath.Join(outside, "secret"), filepath.Join(tree, "d", "ok.txt"))
	}
	defer func() { afterNoLinkWalk = nil }()
	if _, _, err := OpenInTreeNoLinks(tree, "d/ok.txt", 10); !errors.Is(err, ErrSymlink) {
		t.Errorf("leaf swapped after its check: %v", err)
	}
}

func TestRemoveInTreeNoLinksRemovesTheEntryNotTheTarget(t *testing.T) {
	tree, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "keep")
	if err := os.WriteFile(target, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(tree, "l")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveInTreeNoLinks(tree, "l"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("the link target was removed")
	}
}

func TestOpenInTreeNoLinksRefusesAHardLink(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(tree, "a.txt"), filepath.Join(tree, "b.txt")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"a.txt", "b.txt"} {
		if _, _, err := OpenInTreeNoLinks(tree, rel, 10); !errors.Is(err, ErrHardLink) {
			t.Errorf("%s: err = %v, want ErrHardLink", rel, err)
		}
	}
}

// The leaf swapped after its Lstat for something os.Root still opens: only
// the SameFile and IsRegular check after the open refuses it.
func TestOpenInTreeNoLinksRefusesALeafSwappedInPlace(t *testing.T) {
	for name, swap := range map[string]func(d string) error{
		"in-directory link": func(d string) error {
			if err := os.Remove(filepath.Join(d, "ok.txt")); err != nil {
				return err
			}
			return os.Symlink("other.txt", filepath.Join(d, "ok.txt"))
		},
		"sibling file": func(d string) error {
			return os.Rename(filepath.Join(d, "other.txt"), filepath.Join(d, "ok.txt"))
		},
		"fifo": func(d string) error {
			if err := os.Remove(filepath.Join(d, "ok.txt")); err != nil {
				return err
			}
			return syscall.Mkfifo(filepath.Join(d, "ok.txt"), 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			tree := t.TempDir()
			d := filepath.Join(tree, "d")
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, n := range []string{"ok.txt", "other.txt"} {
				if err := os.WriteFile(filepath.Join(d, n), []byte(n), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			afterNoLinkWalk = func() {
				if err := swap(d); err != nil {
					t.Error(err)
				}
			}
			defer func() { afterNoLinkWalk = nil }()
			if f, _, err := OpenInTreeNoLinks(tree, "d/ok.txt", 100); !errors.Is(err, ErrSymlink) {
				if f != nil {
					f.Close()
				}
				t.Fatalf("err = %v, want ErrSymlink", err)
			}
		})
	}
}
