package webpush

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/stevegeek/lever/internal/fsutil"
)

// ErrNotPrivate: a state file is not a regular file of this user that no
// one else can read or write. The push key and the subscriptions (each a
// way to push to a device) live in such files only.
var ErrNotPrivate = errors.New("not a private regular file of this user (want mode 0600)")

// ReadPrivateFile reads at most max bytes of path without following a
// symbolic link, after checking the open file is private.
func ReadPrivateFile(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s: %w", path, ErrNotPrivate)
		}
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := CheckPrivate(fi); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, max)
	}
	return b, nil
}

// CheckPrivate is the rule ReadPrivateFile applies (doctor uses it too).
func CheckPrivate(fi os.FileInfo) error {
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return ErrNotPrivate
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return ErrNotPrivate
	}
	return nil
}

// WritePrivateFile replaces path with data, mode 0600, atomically; a link
// at path is replaced, never followed.
func WritePrivateFile(path string, data []byte) error {
	return fsutil.WriteFileAtomic(path, data, 0o600)
}
