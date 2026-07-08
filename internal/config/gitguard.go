package config

import (
	"os"
	"path/filepath"
)

// treeIsGitRepo reports whether dir itself contains a `.git` entry (file or
// directory) — i.e. dir is a git repository root, not merely somewhere inside
// one. lever's isolation model (R4) targets non-git trees; a tree that is
// itself a git repo is the genuinely-deferred per-worker git case (spec §13)
// and is refused.
//
// This deliberately does NOT walk upward to check ancestors. A tree that is a
// plain subdirectory of a larger git repo (e.g. this repo's own shipped
// example/testdata fixtures, or a deployment tree nested under an operator's
// monorepo checkout) is fine: the pinned Scion's `--workspace` git guard
// (P2 Task 5) plain-mounts the exact dir Scion is given, so an ancestor's
// `.git` is never exposed into the mount. Only the tree's own `.git` matters.
func treeIsGitRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(filepath.Clean(dir), ".git"))
	return err == nil
}
