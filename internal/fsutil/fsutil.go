// Package fsutil holds the stdlib-only file helpers the host packages share.
package fsutil

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to a temp file in the same directory as path
// then renames it over path — atomic on POSIX, so a crash mid-write leaves
// either the old file or the new one, never a torn partial write. The rename
// replaces whatever path was (a symlink included) with a regular file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
