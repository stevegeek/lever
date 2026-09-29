//go:build unix

package hostledger

import (
	"os"
	"syscall"
)

// LockFile takes an exclusive advisory lock (flock) on path, creating it
// 0600 without following a symlink, and returns the unlock. Two processes
// that append to the same record (a broker and the one replacing it on a
// restart) serialise on it.
func LockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|ONoFollow, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
