//go:build linux

package treebackup

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// platformChecksDev is false on Linux: RESOLVE_NO_XDEV refuses every mount
// crossing, including a bind mount with the tree's st_dev, and a btrfs
// subvolume legitimately has a different st_dev without being a mount.
const platformChecksDev = false

// listingWindow covers the coarse clock tick that stamps directory times
// on kernels without fine-grained timestamps.
var listingWindow = 20 * time.Millisecond

// beneath is applied to every open below the root, each of a single name:
// a link (final or not) gives ELOOP and a mount point gives EXDEV.
const beneath = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV | unix.RESOLVE_NO_MAGICLINKS

func sysProbeKernel() error {
	var u unix.Utsname
	if err := unix.Uname(&u); err == nil {
		rel := unix.ByteSliceToString(u.Release[:])
		if major, minor, ok := kernelVersion(rel); ok && (major < 5 || major == 5 && minor < 8) {
			return fmt.Errorf("%w: Linux %s (5.8 or later is required)", ErrUnsupported, rel)
		}
	}
	how := unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS}
	fd, err := openat2(unix.AT_FDCWD, "/", &how)
	switch {
	case err == nil:
		unix.Close(fd)
		return nil
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EPERM):
		// EPERM here is a seccomp filter: nothing else refuses an O_PATH open of "/".
		return fmt.Errorf("%w: openat2 is not available (%v)", ErrUnsupported, err)
	default:
		return fmt.Errorf("tree backups: openat2 probe: %w", err)
	}
}

func kernelVersion(rel string) (int, int, bool) {
	parts := strings.SplitN(rel, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	m := parts[1]
	for i, c := range m {
		if c < '0' || c > '9' {
			m = m[:i]
			break
		}
	}
	minor, err := strconv.Atoi(m)
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

func openat2(dirfd int, name string, how *unix.OpenHow) (int, error) {
	for {
		fd, err := unix.Openat2(dirfd, name, how)
		if err == unix.EINTR {
			continue
		}
		return fd, err
	}
}

func sysOpenRoot(path string) (int, error) {
	return unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// sysProbeRoot proves the root can be searched, through the same openat2
// every later open uses.
func sysProbeRoot(fd int) error {
	how := unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: beneath}
	pfd, err := openat2(fd, ".", &how)
	if err != nil {
		return err
	}
	unix.Close(pfd)
	return nil
}

func sysOpenDir(dirfd int, name string) (int, error) {
	how := unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC, Resolve: beneath}
	return openat2(dirfd, name, &how)
}

func sysOpenListing(dirfd int) (int, error) {
	for {
		fd, err := unix.Openat(dirfd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err == unix.EINTR {
			continue
		}
		return fd, err
	}
}

// sysOpenFile never blocks on a FIFO swapped in after the stat (O_NONBLOCK);
// the caller's fstat rejects anything but a regular file.
func sysOpenFile(dirfd int, name string) (int, error) {
	flags := uint64(unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_NOCTTY | unix.O_CLOEXEC)
	how := unix.OpenHow{Flags: flags | unix.O_NOATIME, Resolve: beneath}
	fd, err := openat2(dirfd, name, &how)
	if err == unix.EPERM {
		// O_NOATIME is refused for a file the caller does not own.
		how.Flags = flags
		fd, err = openat2(dirfd, name, &how)
	}
	return fd, err
}

// sysFstatat uses statx for STATX_ATTR_MOUNT_ROOT: a bind mount of a single
// file is a mount point no open has crossed yet.
func sysFstatat(dirfd int, name string) (sysStat, error) {
	var stx unix.Statx_t
	err := unix.Statx(dirfd, name, unix.AT_SYMLINK_NOFOLLOW|unix.AT_NO_AUTOMOUNT|unix.AT_STATX_SYNC_AS_STAT, unix.STATX_BASIC_STATS, &stx)
	if err != nil {
		return sysStat{}, err
	}
	return sysStat{
		Dev:       unix.Mkdev(stx.Dev_major, stx.Dev_minor),
		Ino:       stx.Ino,
		Mode:      uint32(stx.Mode),
		Size:      int64(stx.Size),
		Mtime:     stx.Mtime.Sec*1e9 + int64(stx.Mtime.Nsec),
		Ctime:     stx.Ctime.Sec*1e9 + int64(stx.Ctime.Nsec),
		Nlink:     uint64(stx.Nlink),
		MountRoot: stx.Attributes_mask&unix.STATX_ATTR_MOUNT_ROOT != 0 && stx.Attributes&unix.STATX_ATTR_MOUNT_ROOT != 0,
	}, nil
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
		Dev:   uint64(st.Dev),
		Ino:   uint64(st.Ino),
		Mode:  uint32(st.Mode),
		Size:  st.Size,
		Mtime: st.Mtim.Nano(),
		Ctime: st.Ctim.Nano(),
		Nlink: uint64(st.Nlink),
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
