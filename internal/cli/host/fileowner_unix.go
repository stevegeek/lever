//go:build unix

package host

import (
	"io/fs"
	"syscall"
)

// fileOwner is the uid that owns fi's file, when the platform says.
func fileOwner(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
