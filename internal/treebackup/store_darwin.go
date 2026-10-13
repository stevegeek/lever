//go:build darwin

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

// syncStoreFS has nothing to do: each new blob was fsynced before its
// rename, and the manifest's F_FULLFSYNC flushes the drive cache.
func syncStoreFS(*os.File) error { return nil }

// syncNewBlob is a plain fsync: os.File.Sync would issue F_FULLFSYNC, which
// costs a drive cache flush per blob.
func syncNewBlob(f *os.File) error { return unix.Fsync(int(f.Fd())) }

func syncManifest(f *os.File) error {
	_, err := unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0)
	return err
}

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
	st := unix.Fstore_t{Flags: unix.F_ALLOCATECONTIG | unix.F_ALLOCATEALL, Posmode: unix.F_PEOFPOSMODE, Length: size - fi.Size()}
	if err := unix.FcntlFstore(f.Fd(), unix.F_PREALLOCATE, &st); err != nil {
		st.Flags = unix.F_ALLOCATEALL
		if err := unix.FcntlFstore(f.Fd(), unix.F_PREALLOCATE, &st); err != nil {
			return err
		}
	}
	return unix.Ftruncate(int(f.Fd()), size)
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

// allocated is the space fi's blocks take on disk.
func allocated(fi fs.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return 0
}

func fileKey(fi fs.FileInfo) (string, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return strconv.FormatUint(uint64(st.Dev), 10) + ":" + strconv.FormatUint(st.Ino, 10), true
}

// isCopyOnWrite: APFS and ZFS write every overwrite to new blocks, so a
// preallocated file's blocks are no reserve.
func isCopyOnWrite(dir *os.File) bool {
	var st unix.Statfs_t
	if unix.Fstatfs(int(dir.Fd()), &st) != nil {
		return false
	}
	name := unix.ByteSliceToString(st.Fstypename[:])
	return name == "apfs" || name == "zfs"
}
