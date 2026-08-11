package wire

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The staging target lives inside the agent's own read-write mount, and the
// host process that writes it runs as the operator. So an agent that replaces
// its `.lever` directory with a symlink is asking the host to write, and chmod
// 0600, wherever the link points. Staging must refuse rather than follow.

func TestStageRefusesASymlinkedStagingDir(t *testing.T) {
	tree := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "lever.yaml")
	if err := os.WriteFile(victim, []byte("name: assistant\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The agent plants the link where its bootstrap dir belongs.
	if err := os.Symlink(outside, filepath.Join(tree, ".lever")); err != nil {
		t.Fatal(err)
	}

	err := Stage(tree, ".lever", Bootstrap{Ticket: "t", AgentCN: "scratch"})
	if err == nil {
		t.Fatal("staging through an agent-planted symlink must fail")
	}
	if got, _ := os.ReadFile(victim); string(got) != "name: assistant\n" {
		t.Fatalf("the host wrote outside the tree: victim is now %q", got)
	}
	if _, serr := os.Stat(filepath.Join(outside, "bootstrap.json")); serr == nil {
		t.Fatal("the host created bootstrap.json outside the tree")
	}
}

// The same trick one level down: the worker's own subdirectory is agent-writable
// too, so `workers/scratch/.lever` may be a link even when the tree root is not.
func TestStageRefusesASymlinkedNestedStagingDir(t *testing.T) {
	tree := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "workers", "scratch"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, "workers", "scratch", ".lever")); err != nil {
		t.Fatal(err)
	}

	if err := Stage(tree, filepath.Join("workers", "scratch", ".lever"), Bootstrap{Ticket: "t"}); err == nil {
		t.Fatal("staging through a nested symlink must fail")
	}
	if _, err := os.Stat(filepath.Join(outside, "bootstrap.json")); err == nil {
		t.Fatal("the host created bootstrap.json outside the tree")
	}
}

// A symlink at the FILE rather than the directory is the same attack.
func TestStageRefusesASymlinkedBootstrapFile(t *testing.T) {
	tree := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "controller.pat")
	if err := os.WriteFile(victim, []byte("pat"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tree, ".lever"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(tree, ".lever", "bootstrap.json")); err != nil {
		t.Fatal(err)
	}

	if err := Stage(tree, ".lever", Bootstrap{Ticket: "t"}); err == nil {
		t.Fatal("staging onto a symlinked bootstrap.json must fail")
	}
	if got, _ := os.ReadFile(victim); string(got) != "pat" {
		t.Fatalf("the host overwrote the link target: %q", got)
	}
}

// An escape by traversal, with no symlink involved.
func TestStageRefusesEscapeByTraversal(t *testing.T) {
	tree := t.TempDir()
	if err := Stage(tree, filepath.Join("..", "escape", ".lever"), Bootstrap{Ticket: "t"}); err == nil {
		t.Fatal("a relative path leaving the tree must fail")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(tree), "escape")); err == nil {
		t.Fatal("the host created a directory outside the tree")
	}
}

// The ordinary path still works, with the modes the enrolment envelope needs.
func TestStageWritesConfinedAndPrivate(t *testing.T) {
	tree := t.TempDir()
	rel := filepath.Join("workers", "scratch", ".lever")
	if err := Stage(tree, rel, Bootstrap{Ticket: "tkt", AgentCN: "scratch"}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	p := filepath.Join(tree, rel, "bootstrap.json")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("bootstrap.json not staged: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("bootstrap.json mode = %#o, want 0600", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(raw), `"ticket":"tkt"`) {
		t.Fatalf("staged content wrong: %q (%v)", raw, err)
	}
}

// A re-stage over an existing file must reassert 0600 — os.WriteFile applies its
// perm only when it CREATES the file, and every resume re-stages a fresh ticket.
func TestStageReassertsModeOnRestage(t *testing.T) {
	tree := t.TempDir()
	if err := Stage(tree, ".lever", Bootstrap{Ticket: "one"}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(tree, ".lever", "bootstrap.json")
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Stage(tree, ".lever", Bootstrap{Ticket: "two"}); err != nil {
		t.Fatalf("re-stage: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("after re-stage mode = %#o, want 0600", fi.Mode().Perm())
	}
}
