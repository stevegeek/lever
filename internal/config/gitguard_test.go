package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTreeIsGitRepo(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !treeIsGitRepo(repo) {
		t.Fatalf("repo root: got false, want true (dir has its own .git)")
	}

	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if treeIsGitRepo(sub) {
		t.Fatalf("plain subdir of a git repo: got true, want false (only an ancestor has .git; the guard no longer walks upward)")
	}

	plain := t.TempDir()
	if treeIsGitRepo(plain) {
		t.Fatalf("plain dir reported as a git repo")
	}
}
