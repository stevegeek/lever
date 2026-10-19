package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// noTreeLinksFixture is an instance root holding a tree an agent writes and
// a host file outside it that the agent must not get read.
func noTreeLinksFixture(t *testing.T) (root, tree, secret string) {
	t.Helper()
	root = t.TempDir()
	tree = filepath.Join(root, "tree")
	for _, d := range []string{tree, filepath.Join(tree, "jobs"), filepath.Join(root, "tasks")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret = filepath.Join(root, "id_ed25519")
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(secret, "PRIVATE KEY")
	write(filepath.Join(tree, "jobs", "42.md"), "job 42")
	write(filepath.Join(root, "tasks", "t.md"), "host task")
	return root, tree, secret
}

func TestReadFileNoTreeLinks(t *testing.T) {
	root, tree, secret := noTreeLinksFixture(t)
	link := func(target, at string) string {
		if err := os.Symlink(target, at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	// The operator's own links outside the tree are followed, also into it.
	ok := map[string]string{
		"in the tree":         filepath.Join(tree, "jobs", "42.md"),
		"outside the tree":    filepath.Join(root, "tasks", "t.md"),
		"operator's link":     link(filepath.Join(root, "tasks", "t.md"), filepath.Join(root, "t-link.md")),
		"operator's dir link": filepath.Join(link(filepath.Join(tree, "jobs"), filepath.Join(root, "jobs-link")), "42.md"),
		"dot-dot":             filepath.Join(tree, "jobs", "..", "jobs", "42.md"),
	}
	for name, p := range ok {
		if _, err := ReadFileNoTreeLinks(p, tree, 1<<10); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A link an agent planted in the tree is refused wherever it points.
	bad := map[string]string{
		"leaf link to a secret":     link(secret, filepath.Join(tree, "jobs", "evil.md")),
		"relative leaf link":        link("../../id_ed25519", filepath.Join(tree, "jobs", "rel.md")),
		"dir link out of the tree":  filepath.Join(link(root, filepath.Join(tree, "up")), "id_ed25519"),
		"in-tree link to tree file": link("42.md", filepath.Join(tree, "jobs", "alias.md")),
		"via operator link to tree": filepath.Join(link(filepath.Join(tree, "up"), filepath.Join(root, "up-link")), "id_ed25519"),
	}
	for name, p := range bad {
		b, err := ReadFileNoTreeLinks(p, tree, 1<<10)
		if !errors.Is(err, ErrSymlink) || strings.Contains(string(b), "PRIVATE") {
			t.Errorf("%s: %q, %v (want ErrSymlink)", name, b, err)
		}
	}
}

func TestReadFileNoTreeLinksRefusesNonFilesAndSize(t *testing.T) {
	root, tree, _ := noTreeLinksFixture(t)
	fifo := filepath.Join(tree, "jobs", "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if _, err := ReadFileNoTreeLinks(fifo, tree, 1<<10); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("fifo: %v", err)
	}
	if _, err := ReadFileNoTreeLinks(filepath.Join(tree, "jobs"), tree, 1<<10); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("dir: %v", err)
	}
	if _, err := ReadFileNoTreeLinks(filepath.Join(root, "tasks", "t.md"), tree, 4); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("size: %v", err)
	}
	if _, err := ReadFileNoTreeLinks(filepath.Join(root, "nope.md"), tree, 4); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	// ReadRegularFile: the same refusals for a path it trusts.
	if _, err := ReadRegularFile(fifo, 1<<10); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("ReadRegularFile fifo: %v", err)
	}
	if _, err := ReadRegularFile(filepath.Join(root, "tasks", "t.md"), 4); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("ReadRegularFile size: %v", err)
	}
}

// A component swapped for a link after its lstat, the directory or the leaf,
// is refused: each open is O_NOFOLLOW and must land on what the lstat saw.
func TestReadFileNoTreeLinksSwapAfterLstat(t *testing.T) {
	for _, at := range []string{"jobs", "42.md"} {
		root, tree, _ := noTreeLinksFixture(t)
		jobs := filepath.Join(tree, "jobs")
		if err := os.WriteFile(filepath.Join(root, "42.md"), []byte("PRIVATE"), 0o600); err != nil {
			t.Fatal(err)
		}
		afterNoTreeLinkStep = func(name string) {
			if name != at {
				return
			}
			victim, target := jobs, root
			if at == "42.md" {
				victim, target = filepath.Join(jobs, "42.md"), filepath.Join(root, "42.md")
			}
			if err := os.Rename(victim, victim+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, victim); err != nil {
				t.Fatal(err)
			}
		}
		b, err := ReadFileNoTreeLinks(filepath.Join(jobs, "42.md"), tree, 1<<10)
		afterNoTreeLinkStep = nil
		if !errors.Is(err, ErrSymlink) || strings.Contains(string(b), "PRIVATE") {
			t.Fatalf("%s swapped: %q, %v", at, b, err)
		}
	}
}

// Swapped back and forth while it is read, an in-tree directory never
// yields the file behind the link.
func TestReadFileNoTreeLinksRace(t *testing.T) {
	root, tree, _ := noTreeLinksFixture(t)
	jobs := filepath.Join(tree, "jobs")
	if err := os.WriteFile(filepath.Join(root, "42.md"), []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
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
			_ = os.Rename(jobs, jobs+".old")
			_ = os.Symlink(root, jobs)
			_ = os.Remove(jobs)
			_ = os.Rename(jobs+".old", jobs)
		}
	}()
	for i := 0; i < 20000; i++ {
		if b, _ := ReadFileNoTreeLinks(filepath.Join(jobs, "42.md"), tree, 1<<10); strings.Contains(string(b), "PRIVATE") {
			close(stop)
			<-done
			t.Fatalf("read %d leaked the file behind the link", i)
		}
	}
	close(stop)
	<-done
}
