package fsutil

import (
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
