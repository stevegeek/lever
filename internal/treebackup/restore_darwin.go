package treebackup

import (
	"golang.org/x/sys/unix"
)

const noFollowAny = unix.O_NOFOLLOW_ANY

// setTimes uses futimes(2), whose microsecond resolution drops the last
// three digits of the recorded mtime; x/sys has no futimens on darwin, and
// a path-based call would leave the fd chain.
func (sysFS) setTimes(fd int, mtime int64) error {
	tv := unix.NsecToTimeval(mtime)
	return unix.Futimes(fd, []unix.Timeval{tv, tv})
}

func (sysFS) renameNoReplace(dirfd int, from, to string) error {
	return unix.RenameatxNp(dirfd, from, dirfd, to, unix.RENAME_EXCL)
}

func (sysFS) freeBytes(fd int) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(fd, &st); err != nil {
		return 0, err
	}
	return clampMul(st.Bavail, uint64(st.Bsize)), nil
}
