//go:build linux

package treebackup

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

const platformSupported = true

const oNoFollow = unix.O_NOFOLLOW

// syncStoreFS flushes every blob written since the last commit. syncfs
// reports writeback errors from Linux 5.8 on, so one call replaces an fsync
// per blob.
func syncStoreFS(dir *os.File) error { return unix.Syncfs(int(dir.Fd())) }

func syncNewBlob(*os.File) error { return nil }

func syncManifest(f *os.File) error { return unix.Fsync(int(f.Fd())) }

func fsync(f *os.File) error { return unix.Fsync(int(f.Fd())) }

// preallocate makes f exactly size bytes, with the blocks allocated.
func preallocate(f *os.File, size int64) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() == size {
		return nil
	}
	if fi.Size() > size || size == 0 {
		return unix.Ftruncate(int(f.Fd()), size)
	}
	return unix.Fallocate(int(f.Fd()), 0, 0, size)
}

func fsFree(dir *os.File) (freeBytes, freeInodes, totalInodes uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(dir.Fd()), &st); err != nil {
		return 0, 0, 0, err
	}
	return st.Bavail * uint64(st.Bsize), st.Ffree, st.Files, nil
}

func flockTry(f *os.File, exclusive bool) error {
	how := unix.LOCK_SH | unix.LOCK_NB
	if exclusive {
		how = unix.LOCK_EX | unix.LOCK_NB
	}
	for {
		err := unix.Flock(int(f.Fd()), how)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EWOULDBLOCK):
			return errWouldBlock
		default:
			return err
		}
	}
}

func flockUnlock(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }

func fileKey(fi fs.FileInfo) (string, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return strconv.FormatUint(uint64(st.Dev), 10) + ":" + strconv.FormatUint(st.Ino, 10), true
}
