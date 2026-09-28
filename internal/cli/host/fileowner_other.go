//go:build !unix

package host

import "io/fs"

// fileOwner is unknown off unix.
func fileOwner(fs.FileInfo) (int, bool) { return 0, false }
