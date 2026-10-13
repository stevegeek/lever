//go:build unix

package treebackup

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// statIdentity converts st_dev the way the walker does, or the guard could
// never match on darwin, where st_dev is a signed 32-bit value.
func statIdentity(path string) (dev, ino uint64, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("%s: no file identity", path)
	}
	dev = uint64(st.Dev)
	if runtime.GOOS == "darwin" {
		dev = uint64(uint32(st.Dev))
	}
	return dev, uint64(st.Ino), nil
}
