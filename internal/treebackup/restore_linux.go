package treebackup

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

const noFollowAny = 0

// setTimes is futimens(2): utimensat with a NULL path, which x/sys does not
// expose.
func (sysFS) setTimes(fd int, mtime int64) error {
	ts := [2]unix.Timespec{unix.NsecToTimespec(mtime), unix.NsecToTimespec(mtime)}
	_, _, e := unix.Syscall6(unix.SYS_UTIMENSAT, uintptr(fd), 0, uintptr(unsafe.Pointer(&ts)), 0, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

func (sysFS) renameNoReplace(dirfd int, from, to string) error {
	return unix.Renameat2(dirfd, from, dirfd, to, unix.RENAME_NOREPLACE)
}

func (sysFS) freeBytes(fd int) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(fd, &st); err != nil {
		return 0, err
	}
	return clampMul(st.Bavail, uint64(st.Bsize)), nil
}
