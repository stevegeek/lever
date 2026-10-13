//go:build linux || darwin

package fsutil

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	// maxIdentityLevels bounds the ".." chain. The chain is one path string,
	// so PATH_MAX ends it first in practice, with an error.
	maxIdentityLevels = 4096
	// maxIdentityLinks is the kernel's own limit on links followed while
	// looking for inner's deepest existing ancestor.
	maxIdentityLinks = 40
)

// statOuter is a test seam: Linux test filesystems are case-sensitive, so a
// case alias of outer is simulated by mapping the alias spelling here.
var statOuter = func(path string, st *unix.Stat_t) error { return unix.Stat(path, st) }

// InsideByIdentity reports whether inner is outer or lies below it. It
// compares outer's (dev, ino) with every directory on inner's ".." chain,
// walked with fstatat from one directory fd, so no spelling of either path
// (a symlink, a case alias, a firmlink, a bind mount of outer) hides the
// relation. Only search permission is needed on inner's ancestors.
//
// When inner does not exist, the answer is for its deepest existing
// ancestor: where inner would be created. An outer that does not exist
// contains nothing. Any other failure is an error, and callers must treat
// it as "inside": lever cannot prove the opposite.
//
// ".." at a mount root leads to the mount point's parent, so a directory of
// outer bind-mounted elsewhere has a chain that never passes outer. On
// Linux 5.8 and later each mount root on the chain is looked up in
// /proc/self/mountinfo, and a mount of a directory at or below outer
// counts as inside (see mountRootInside). Elsewhere that case is not seen.
func InsideByIdentity(inner, outer string) (bool, error) {
	var o unix.Stat_t
	if err := statOuter(outer, &o); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", outer, err)
	}
	fd, dir, err := openDeepestDir(inner)
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	return insideFrom(fd, dir, outer, &o)
}

// InsideByIdentityFD is InsideByIdentity for the directory open as dirfd:
// the answer is about the directory the fd holds, whatever its path names
// now. name is used in errors only.
func InsideByIdentityFD(dirfd int, name, outer string) (bool, error) {
	var o unix.Stat_t
	if err := statOuter(outer, &o); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", outer, err)
	}
	return insideFrom(dirfd, name, outer, &o)
}

// insideFrom walks up from the directory open as fd, one fd per level:
// each parent is opened as ".." of the directory before it, never as a
// "../.." path from the start, which an agent renaming an ancestor between
// two levels could send around outer.
func insideFrom(fd int, dir, outer string, o *unix.Stat_t) (bool, error) {
	mounts := &mountCheck{outer: outer}
	cur, err := statChain(fd)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", dir, err)
	}
	curfd, owned := fd, -1
	defer func() {
		if owned >= 0 {
			unix.Close(owned)
		}
	}()
	for level := 1; level <= maxIdentityLevels; level++ {
		if sameIdentity(&cur.st, o) {
			return true, nil
		}
		pfd, err := openParentDir(curfd)
		if err != nil {
			return false, fmt.Errorf("open ancestor %d of %s: %w", level, dir, err)
		}
		parent, err := statChain(pfd)
		if err != nil {
			unix.Close(pfd)
			return false, fmt.Errorf("stat ancestor %d of %s: %w", level, dir, err)
		}
		top := sameIdentity(&parent.st, &cur.st)
		if cur.hasMnt && (top || parent.mnt != cur.mnt) {
			in, err := mounts.rootInside(cur.mnt)
			if err != nil || in || top {
				unix.Close(pfd)
				if err != nil {
					return false, fmt.Errorf("mount above %s: %w", dir, err)
				}
				return in, nil
			}
		}
		if top {
			unix.Close(pfd)
			return false, nil
		}
		if owned >= 0 {
			unix.Close(owned)
		}
		curfd, owned, cur = pfd, pfd, parent
	}
	return false, fmt.Errorf("%s: more than %d ancestors", dir, maxIdentityLevels)
}

func sameIdentity(a, b *unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino
}

// openDeepestDir opens p, or its deepest existing ancestor, as a directory
// fd. A link as the last component is followed even when it dangles, so the
// answer is about where the kernel would create p; every other component is
// left to the kernel.
func openDeepestDir(p string) (int, string, error) {
	links := 0
	for {
		fd, err := openIdentityDir(p)
		if err == nil {
			return fd, p, nil
		}
		if !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENOTDIR) {
			return -1, "", fmt.Errorf("open %s: %w", p, err)
		}
		var lst unix.Stat_t
		if unix.Lstat(p, &lst) == nil && lst.Mode&unix.S_IFMT == unix.S_IFLNK {
			if links++; links > maxIdentityLinks {
				return -1, "", fmt.Errorf("open %s: %w", p, unix.ELOOP)
			}
			target, err := readlinkPath(p)
			if err != nil {
				return -1, "", fmt.Errorf("readlink %s: %w", p, err)
			}
			if !strings.HasPrefix(target, "/") {
				target = parentDir(p) + "/" + target
			}
			p = target
			continue
		}
		parent := parentDir(p)
		if parent == p {
			return -1, "", fmt.Errorf("open %s: %w", p, err)
		}
		p = parent
	}
}

func readlinkPath(p string) (string, error) {
	buf := make([]byte, 4096)
	n, err := unix.Readlink(p, buf)
	if err != nil {
		return "", err
	}
	if n == len(buf) {
		return "", unix.ENAMETOOLONG
	}
	return string(buf[:n]), nil
}

// parentDir drops p's last component without cleaning the rest, so the
// kernel still resolves every ".." and link that remains. It returns p
// itself when there is nothing left to drop.
func parentDir(p string) string {
	t := strings.TrimRight(p, "/")
	if t == "" {
		if p == "" {
			return "."
		}
		return "/"
	}
	i := strings.LastIndexByte(t, '/')
	switch {
	case i < 0:
		return "."
	case i == 0:
		return "/"
	}
	return t[:i]
}
