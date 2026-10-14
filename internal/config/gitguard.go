package config

import (
	"os"
	"path/filepath"
)

// treeIsGitRepo reports whether dir itself has a `.git` entry. Ancestors are
// not checked: scion mounts the exact dir, so an ancestor's `.git` is never
// exposed.
func treeIsGitRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(filepath.Clean(dir), ".git"))
	return err == nil
}
