package broker

import (
	"os"
	"path/filepath"
	"testing"
)

// A manager that hard-links its bootstrap.json to a worker's file gets the
// entry replaced: the worker's file never receives the manager's ticket.
func TestManagerFreshTicketDoesNotWriteThroughAHardLink(t *testing.T) {
	tree := t.TempDir()
	dir := filepath.Join(tree, ".lever")
	other := filepath.Join(tree, "workers", "w1", "notes.txt")
	for _, d := range []string{dir, filepath.Dir(other)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(other, []byte("worker data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(other, filepath.Join(dir, "bootstrap.json")); err != nil {
		t.Fatal(err)
	}
	b := New(testConfig(t, withManager("test-manager", "appname"), func(c *Config) { c.Dispatch.Tree = tree }))
	if err := b.stageFreshTicket("test-manager", dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(other); string(got) != "worker data" {
		t.Fatalf("the worker's file now holds %q", got)
	}
	fi, err := os.Lstat(filepath.Join(dir, "bootstrap.json"))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 || os.SameFile(fi, mustStat(t, other)) {
		t.Fatalf("bootstrap.json = %v, %v; want its own 0600 file", fi, err)
	}
}

// A symlinked staging dir, in the tree or not, is refused.
func TestManagerFreshTicketRefusesASymlinkedDir(t *testing.T) {
	tree, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(tree, ".lever")); err != nil {
		t.Fatal(err)
	}
	b := New(testConfig(t, withManager("test-manager", "appname"), func(c *Config) { c.Dispatch.Tree = tree }))
	if err := b.stageFreshTicket("test-manager", filepath.Join(tree, ".lever")); err == nil {
		t.Fatal("staging through a symlinked .lever succeeded")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("link target gained %d entries", len(ents))
	}
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}
