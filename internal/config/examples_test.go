package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("cannot locate test file")
	}
	// this file: <repo>/internal/config/examples_test.go → repo root is two dirs up
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

func TestShippedExamplesLoadAndValidate(t *testing.T) {
	root := repoRoot(t)
	for _, name := range []string{"hello-worker", "two-agents-comms", "multi-project"} {
		src := filepath.Join(root, "examples", name, "lever.yaml")
		body, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("example %s: read %s: %v", name, src, err)
		}
		// Copy into a fresh non-repo temp dir before Load: the shipped example
		// lives inside this project's own git checkout, and validateNonGitTree
		// (added for P2) now rejects a tree nested under a `.git` ancestor. That
		// guard targets real deployments, not the in-repo example fixtures, so
		// exercise the config shape from a git-free location (t.TempDir() is
		// never inside a repo) rather than weakening the guard for this test.
		dst := filepath.Join(t.TempDir(), "lever.yaml")
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			t.Fatalf("example %s: write copy: %v", name, err)
		}
		app, err := Load(dst)
		if err != nil {
			t.Fatalf("example %s: Load failed: %v", name, err)
		}
		if app.Name != name {
			t.Errorf("example %s: name=%q", name, app.Name)
		}
		if len(app.Workers) == 0 {
			t.Errorf("example %s: no workers", name)
		}
	}
}
