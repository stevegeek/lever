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
