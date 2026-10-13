//go:build darwin

package treebackup

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

// platformChecksDev is true on darwin: there is no RESOLVE_NO_XDEV, so a
// mount is told by an st_dev different from the root's, checked on the
// fstatat result before the open and on the fd after it.
const platformChecksDev = true

// listingWindow covers HFS+, which stamps directory times in whole seconds.
var listingWindow = time.Second

// oSearch is O_EXEC|O_DIRECTORY: a directory fd that can be searched but not
// listed, for 0300 directories. x/sys does not define it.
const oSearch = 0x40100000

// noFollow is set on every open below the root: O_NOFOLLOW_ANY refuses a
// link in any component.
const noFollow = unix.O_NOFOLLOW | unix.O_NOFOLLOW_ANY

func sysProbeKernel() error { return nil }

func sysOpenRoot(path string) (int, error) {
	fd, err := retryEINTR(func() (int, error) {
		return unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	})
	if err == unix.EACCES {
		fd, err = retryEINTR(func() (int, error) {
			return unix.Open(path, oSearch|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		})
	}
	return fd, err
}

// sysProbeRoot proves the root can be searched: a lookup of "." needs search
// permission on the directory.
func sysProbeRoot(fd int) error {
	var st unix.Stat_t
	return unix.Fstatat(fd, ".", &st, 0)
}

func sysOpenDir(dirfd int, name string) (int, error) {
	fd, err := retryEINTR(func() (int, error) {
		return unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|noFollow|unix.O_CLOEXEC, 0)
	})
	if err == unix.EACCES {
		fd, err = retryEINTR(func() (int, error) {
			return unix.Openat(dirfd, name, oSearch|noFollow|unix.O_CLOEXEC, 0)
		})
	}
	return fd, err
}

func sysOpenListing(dirfd int) (int, error) {
	return retryEINTR(func() (int, error) {
		return unix.Openat(dirfd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	})
}

// sysOpenFile never blocks on a FIFO swapped in after the stat (O_NONBLOCK);
// the caller's fstat rejects anything but a regular file.
func sysOpenFile(dirfd int, name string) (int, error) {
	return retryEINTR(func() (int, error) {
		return unix.Openat(dirfd, name, unix.O_RDONLY|noFollow|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	})
}

func sysFstatat(dirfd int, name string) (sysStat, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return sysStat{}, err
	}
	return fromUnixStat(&st), nil
}

func sysFstat(fd int) (sysStat, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return sysStat{}, err
	}
	return fromUnixStat(&st), nil
}

func sysLstat(path string) (sysStat, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return sysStat{}, err
	}
	return fromUnixStat(&st), nil
}

func fromUnixStat(st *unix.Stat_t) sysStat {
	return sysStat{
		Dev:      uint64(uint32(st.Dev)),
		Ino:      st.Ino,
		Mode:     uint32(st.Mode),
		Size:     st.Size,
		Mtime:    st.Mtim.Nano(),
		Ctime:    st.Ctim.Nano(),
		Nlink:    uint64(st.Nlink),
		Dataless: st.Flags&unix.SF_DATALESS != 0,
	}
}

func sysReadlinkat(dirfd int, name string, buf []byte) (int, error) {
	return unix.Readlinkat(dirfd, name, buf)
}

func sysPread(fd int, p []byte, off int64) (int, error) {
	for {
		n, err := unix.Pread(fd, p, off)
		if err == unix.EINTR {
			continue
		}
		return n, err
	}
}

func sysClose(fd int) { unix.Close(fd) }

func classify(err error) errClass {
	var errno unix.Errno
	if !errors.As(err, &errno) {
		return errOther
	}
	switch errno {
	case unix.ENOENT:
		return errGone
	case unix.ELOOP:
		return errLoop
	case unix.EXDEV:
		return errCrossDev
	case unix.ENOTDIR:
		return errNotDir
	case unix.ENXIO:
		return errNoDevice
	case unix.EACCES, unix.EPERM:
		return errDenied
	default:
		return errOther
	}
}

func isNoSpace(err error) bool {
	return errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT)
}
