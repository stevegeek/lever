package config

import (
	"os"
	"path/filepath"
)

// treeInsideGitRepo walks upward from dir; if dir or any ancestor contains a
// `.git` entry (file or directory), it returns that repo root and true. lever's
// isolation model (R4) targets non-git trees — the guard keeps an operator from
// silently running the single-project model against a git working tree, whose
// per-worker git workflow is deferred (spec §13).
func treeInsideGitRepo(dir string) (string, bool) {
	d := filepath.Clean(dir)
	for {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false
		}
		d = parent
	}
}
