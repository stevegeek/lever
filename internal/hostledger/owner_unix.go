//go:build unix

package hostledger

import (
	"io/fs"
	"syscall"
)

// FileOwner is the uid that owns fi, where the platform reports one.
func FileOwner(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// ONoFollow is O_NOFOLLOW where the platform has it.
const ONoFollow = syscall.O_NOFOLLOW

// FileID is fi's inode number, where the platform reports one: a rotation
// replaces the file under a path, and the id is how a reader notices.
func FileID(fi fs.FileInfo) uint64 {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(st.Ino)
}
