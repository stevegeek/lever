//go:build linux || darwin

package fsutil

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// maxPathLinks bounds the symbolic links ReadFileNoTreeLinks follows, as the
// kernel bounds its own.
const maxPathLinks = 40

// afterNoTreeLinkStep is a test seam run after each component's lstat and
// before it is opened or followed.
var afterNoTreeLinkStep func(name string)

// ReadFileNoTreeLinks is ReadRegularFile for a path the host user names that
// may lead through a directory an agent can write (tree). A symbolic link is
// followed only when it sits outside tree: one inside it is ErrSymlink,
// since an agent could aim it at any host file the user can read. The path
// is walked one component at a time from directory fds, each component
// lstat-ed, then opened with O_NOFOLLOW and checked to be what the lstat
// saw, so a component swapped for a link at any point is refused. A
// directory whose place lever cannot tell counts as inside.
func ReadFileNoTreeLinks(path, tree string, max int64) ([]byte, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir, err := openDirAt(unix.AT_FDCWD, "/")
	if err != nil {
		return nil, err
	}
	defer func() { unix.Close(dir) }()
	todo := components(abs)
	links := 0
	for len(todo) > 0 {
		name := todo[0]
		todo = todo[1:]
		var st unix.Stat_t
		if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return nil, &fs.PathError{Op: "stat", Path: path, Err: err}
		}
		if afterNoTreeLinkStep != nil {
			afterNoTreeLinkStep(name)
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			if links++; links > maxPathLinks {
				return nil, fmt.Errorf("%s: too many symbolic links", path)
			}
			if inside, err := InsideByIdentityFD(dir, path, tree); err != nil || inside {
				return nil, fmt.Errorf("%s: a symbolic link in a directory agents can write: %w", path, ErrSymlink)
			}
			target, err := readlinkAt(dir, name)
			if err != nil {
				return nil, &fs.PathError{Op: "readlink", Path: path, Err: err}
			}
			if strings.HasPrefix(target, "/") {
				root, err := openDirAt(unix.AT_FDCWD, "/")
				if err != nil {
					return nil, err
				}
				unix.Close(dir)
				dir = root
			}
			todo = append(components(target), todo...)
		case unix.S_IFDIR:
			next, err := openDirAt(dir, name)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, swapped(err))
			}
			if !sameStat(next, &st) {
				unix.Close(next)
				return nil, fmt.Errorf("%s changed while it was read: %w", path, ErrSymlink)
			}
			unix.Close(dir)
			dir = next
		case unix.S_IFREG:
			if len(todo) > 0 {
				return nil, &fs.PathError{Op: "open", Path: path, Err: unix.ENOTDIR}
			}
			fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, swapped(err))
			}
			f := os.NewFile(uintptr(fd), path)
			defer f.Close()
			if !sameStat(fd, &st) {
				return nil, fmt.Errorf("%s changed while it was read: %w", path, ErrSymlink)
			}
			return readOpenRegular(f, path, max)
		default:
			if len(todo) > 0 {
				return nil, &fs.PathError{Op: "open", Path: path, Err: unix.ENOTDIR}
			}
			return nil, fmt.Errorf("%s: %w", path, ErrNotRegularFile)
		}
	}
	return nil, fmt.Errorf("%s: a directory: %w", path, ErrNotRegularFile)
}

// components splits p into its names, dropping empty and "." ones.
func components(p string) []string {
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return out
}

// swapped reads an O_NOFOLLOW open that met a link (ELOOP, or ENOTDIR for a
// directory open) as the swap it is.
func swapped(err error) error {
	if err == unix.ELOOP || err == unix.ENOTDIR {
		return ErrSymlink
	}
	return err
}

func sameStat(fd int, want *unix.Stat_t) bool {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false
	}
	return st.Dev == want.Dev && st.Ino == want.Ino
}

func readlinkAt(dir int, name string) (string, error) {
	buf := make([]byte, unix.PathMax)
	n, err := unix.Readlinkat(dir, name, buf)
	if err != nil {
		return "", err
	}
	if n >= len(buf) {
		return "", unix.ENAMETOOLONG
	}
	return string(buf[:n]), nil
}
