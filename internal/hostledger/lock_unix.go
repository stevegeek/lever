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
	f, err := lockFD(path, how)
	if err != nil {
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// maxLockHolder bounds the holder TryLockFileAs writes and LockHolder reads.
const maxLockHolder = 256

// TryLockFileAs is TryLockFile that leaves holder in the file while it holds
// the lock, so a caller it turns away can name what runs (LockHolder).
func TryLockFileAs(path, holder string) (func(), error) {
	f, err := lockFD(path, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return nil, err
	}
	if len(holder) > maxLockHolder {
		holder = holder[:maxLockHolder]
	}
	if f.Truncate(0) == nil {
		_, _ = f.WriteAt([]byte(holder), 0)
	}
	return func() {
		_ = f.Truncate(0)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// LockHolder is what the process holding path's lock wrote there with
// TryLockFileAs, at most maxLockHolder bytes; "" when there is none. It is a
// hint for a message, never a decision: the writer may be between its lock
// and its write.
func LockHolder(path string) string {
	f, err := os.OpenFile(path, os.O_RDONLY|ONoFollow|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	b := make([]byte, maxLockHolder)
	n, _ := f.ReadAt(b, 0)
	return string(b[:n])
}

func lockFD(path string, how int) (*os.File, error) {
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
	return f, nil
}
