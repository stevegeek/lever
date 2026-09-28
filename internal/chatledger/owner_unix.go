//go:build unix

package chatledger

import (
	"io/fs"
	"syscall"
)

func fileOwner(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

const oNoFollow = syscall.O_NOFOLLOW
