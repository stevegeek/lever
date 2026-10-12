//go:build !unix

package voice

import "io/fs"

// fileOwner is unknown off unix.
func fileOwner(fs.FileInfo) (int, bool) { return 0, false }
