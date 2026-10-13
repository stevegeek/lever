//go:build linux || darwin

package treebackup

import (
	"golang.org/x/sys/unix"
)

type sysFS struct{}

func newSysFS() (restoreFS, error) { return sysFS{}, nil }

func toStat(st *unix.Stat_t) fsStat {
	return fsStat{
		id:  fsID{dev: uint64(st.Dev), ino: uint64(st.Ino)},
		dir: st.Mode&unix.S_IFMT == unix.S_IFDIR,
	}
}

func (sysFS) openParent(path string) (int, error) {
	return retryEINTR(func() (int, error) {
		return unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	})
}

func (sysFS) stat(path string) (fsStat, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return fsStat{}, err
	}
	return toStat(&st), nil
}

func (sysFS) fstat(fd int) (fsStat, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fsStat{}, err
	}
	return toStat(&st), nil
}

func (sysFS) lstatat(dirfd int, name string) (fsStat, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fsStat{}, err
	}
	return toStat(&st), nil
}

func (sysFS) mkdirat(dirfd int, name string) error {
	return unix.Mkdirat(dirfd, name, 0o700)
}

func (sysFS) openDir(dirfd int, name string) (int, error) {
	return retryEINTR(func() (int, error) {
		return unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|noFollowAny, 0)
	})
}

func (sysFS) createFile(dirfd int, name string) (int, error) {
	return retryEINTR(func() (int, error) {
		return unix.Openat(dirfd, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|noFollowAny, 0o600)
	})
}

func (sysFS) write(fd int, b []byte) (int, error) {
	return retryEINTR(func() (int, error) { return unix.Write(fd, b) })
}

func (sysFS) writeAt(fd int, b []byte, off int64) (int, error) {
	return retryEINTR(func() (int, error) { return unix.Pwrite(fd, b, off) })
}

func (sysFS) truncate(fd int, size int64) error { return unix.Ftruncate(fd, size) }

func (sysFS) unlinkat(dirfd int, name string) error { return unix.Unlinkat(dirfd, name, 0) }

func (sysFS) symlinkat(target string, dirfd int, name string) error {
	return unix.Symlinkat(target, dirfd, name)
}

func (sysFS) linkat(olddirfd int, oldpath string, newdirfd int, newname string) error {
	return unix.Linkat(olddirfd, oldpath, newdirfd, newname, 0)
}

func (sysFS) setLinkTimes(dirfd int, name string, mtime int64) error {
	ts := unix.NsecToTimespec(mtime)
	return unix.UtimesNanoAt(dirfd, name, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW)
}

func (sysFS) chmod(fd int, mode uint32) error { return unix.Fchmod(fd, mode) }

func (sysFS) close(fd int) error { return unix.Close(fd) }
