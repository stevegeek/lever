package fsutil

import "golang.org/x/sys/unix"

// openIdentityDir opens an O_PATH fd: it needs only search permission on
// the path, and fstatat through it needs no read permission anywhere.
func openIdentityDir(p string) (int, error) {
	return unix.Open(p, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}
