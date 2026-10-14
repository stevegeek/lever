//go:build unix

package hostledger

import (
	"errors"
	"os"
	"syscall"
)

// ErrLocked is TryLockFile's answer when another process holds the lock.
var ErrLocked = errors.New("locked by another process")

// LockFile takes an exclusive advisory lock (flock) on path, creating it
// 0600 without following a symlink, and returns the unlock. Two processes
// that append to the same record (a broker and the one replacing it on a
// restart) serialise on it.
func LockFile(path string) (func(), error) {
	return lockFile(path, syscall.LOCK_EX)
}

// TryLockFile is LockFile without the wait: ErrLocked while another process
// holds it.
func TryLockFile(path string) (func(), error) {
	return lockFile(path, syscall.LOCK_EX|syscall.LOCK_NB)
}

func lockFile(path string, how int) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|ONoFollow, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
