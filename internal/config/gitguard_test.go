package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTreeInsideGitRepo(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if root, inside := treeInsideGitRepo(sub); !inside || root != repo {
		t.Fatalf("nested subdir: got (%q,%v), want (%q,true)", root, inside, repo)
	}
	if root, inside := treeInsideGitRepo(repo); !inside || root != repo {
		t.Fatalf("repo root: got (%q,%v), want (%q,true)", root, inside, repo)
	}
	plain := t.TempDir()
	if _, inside := treeInsideGitRepo(plain); inside {
		t.Fatalf("plain dir reported inside a git repo")
	}
}
