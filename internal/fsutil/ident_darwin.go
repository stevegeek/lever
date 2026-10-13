package fsutil

import (
	"errors"

	"golang.org/x/sys/unix"
)

// oSearch is darwin's O_SEARCH (O_EXEC|O_DIRECTORY), which x/sys does not
// export. It opens a directory the caller may search but not read.
const oSearch = 0x40100000

// openIdentityDir has no O_PATH on darwin: a directory that cannot be read
// (0711, 0311) is opened for search instead. O_NONBLOCK keeps a FIFO at p
// from hanging the open.
func openIdentityDir(p string) (int, error) {
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.EACCES) {
		fd, err = unix.Open(p, oSearch|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	}
	return fd, err
}

// chainStat is one directory of a ".." chain. darwin has no mount ids, and
// no bind mounts of its own to look for.
type chainStat struct {
	st     unix.Stat_t
	mnt    uint64
	hasMnt bool
}

func statChain(fd int) (chainStat, error) {
	var c chainStat
	err := unix.Fstat(fd, &c.st)
	return c, err
}

func openParentDir(fd int) (int, error) {
	pfd, err := unix.Openat(fd, "..", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.EACCES) {
		pfd, err = unix.Openat(fd, "..", oSearch|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	}
	return pfd, err
}

type mountCheck struct{ outer string }

func (*mountCheck) rootInside(uint64) (bool, error) { return false, nil }
