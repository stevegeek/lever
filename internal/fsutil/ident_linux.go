package fsutil

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// openIdentityDir opens an O_PATH fd: it needs only search permission on
// the path, and fstatat through it needs no read permission anywhere.
func openIdentityDir(p string) (int, error) {
	return unix.Open(p, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}

// chainStat is one directory of a ".." chain: its stat and, when the kernel
// reports it (5.8 and later), the id of the mount it is in.
type chainStat struct {
	st     unix.Stat_t
	mnt    uint64
	hasMnt bool
}

func statChain(fd int, name string) (chainStat, error) {
	var c chainStat
	if err := unix.Fstatat(fd, name, &c.st, 0); err != nil {
		return c, err
	}
	var x unix.Statx_t
	if err := unix.Statx(fd, name, unix.AT_STATX_SYNC_AS_STAT, unix.STATX_MNT_ID, &x); err == nil && x.Mask&unix.STATX_MNT_ID != 0 {
		c.mnt, c.hasMnt = x.Mnt_id, true
	}
	return c, nil
}

// mountCheck answers, for the mount roots met on one ".." chain, whether the
// mount shows a directory at or below outer. It reads mountinfo once.
type mountCheck struct {
	outer  string
	mounts map[uint64]mountInfo
	// outerDev and outerPath place outer in its filesystem: the
	// superblock's device and the path from that filesystem's root.
	outerDev, outerPath string
	err                 error
}

type mountInfo struct{ dev, root, point string }

// mountinfoPath is a test seam.
var mountinfoPath = "/proc/self/mountinfo"

// rootInside reports whether the mount with this id is a mount of outer
// or of a directory below it: a bind mount of part of the tree. A mount of
// a filesystem's root ("/") never is, since its chain goes on to that root
// and would have passed outer. Only the case it cannot settle (an unknown
// mount, an outer it cannot place) is an error.
func (m *mountCheck) rootInside(id uint64) (bool, error) {
	if m.mounts == nil && m.err == nil {
		m.mounts, m.err = readMountinfo(mountinfoPath)
	}
	if m.err != nil {
		return false, m.err
	}
	mi, ok := m.mounts[id]
	if !ok {
		return false, fmt.Errorf("mount %d is not in %s", id, mountinfoPath)
	}
	if mi.root == "/" {
		return false, nil
	}
	if m.outerPath == "" {
		if err := m.placeOuter(); err != nil {
			m.err = err
			return false, err
		}
	}
	if mi.dev != m.outerDev {
		return false, nil
	}
	return m.outerPath == "/" || mi.root == m.outerPath || strings.HasPrefix(mi.root, m.outerPath+"/"), nil
}

func (m *mountCheck) placeOuter() error {
	real, err := filepath.EvalSymlinks(m.outer)
	if err != nil {
		return err
	}
	var x unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, real, unix.AT_STATX_SYNC_AS_STAT, unix.STATX_MNT_ID, &x); err != nil {
		return fmt.Errorf("statx %s: %w", real, err)
	}
	if x.Mask&unix.STATX_MNT_ID == 0 {
		return fmt.Errorf("statx %s: no mount id", real)
	}
	om, ok := m.mounts[x.Mnt_id]
	if !ok {
		return fmt.Errorf("the mount of %s is not in %s", real, mountinfoPath)
	}
	var rel string
	switch {
	case om.point == "/":
		rel = real
	case real == om.point:
		rel = "/"
	case strings.HasPrefix(real, om.point+"/"):
		rel = real[len(om.point):]
	default:
		return fmt.Errorf("%s is not below its mount point %s", real, om.point)
	}
	m.outerDev, m.outerPath = om.dev, path.Join(om.root, rel)
	return nil
}

// readMountinfo parses proc(5)'s mountinfo: "id parent major:minor root
// mount-point ...", with space, tab, newline and backslash in paths written
// as octal escapes.
func readMountinfo(name string) (map[uint64]mountInfo, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make(map[uint64]mountInfo)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			return nil, fmt.Errorf("%s: short line %q", name, sc.Text())
		}
		id, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: bad mount id %q", name, fields[0])
		}
		root, err1 := unescapeMountinfo(fields[3])
		point, err2 := unescapeMountinfo(fields[4])
		if err := errors.Join(err1, err2); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[id] = mountInfo{dev: fields[2], root: root, point: point}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func unescapeMountinfo(s string) (string, error) {
	if !strings.Contains(s, `\`) {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+4 > len(s) {
			return "", fmt.Errorf("bad escape in %q", s)
		}
		v, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
		if err != nil {
			return "", fmt.Errorf("bad escape in %q", s)
		}
		b.WriteByte(byte(v))
		i += 3
	}
	return b.String(), nil
}
