//go:build !linux && !darwin

package fsutil

import "errors"

// InsideByIdentity reports whether inner is outer or lies below it. It is
// implemented on Linux and macOS only; elsewhere it always fails, and
// callers treat that as "inside".
func InsideByIdentity(inner, outer string) (bool, error) {
	return false, errors.New("identity checks are not supported on this platform")
}

// ReadFileNoTreeLinks is implemented on Linux and macOS only; elsewhere it
// always fails, since lever cannot place a link outside the tree.
func ReadFileNoTreeLinks(path, tree string, max int64) ([]byte, error) {
	return nil, errors.New("reading a file outside the tree's links is not supported on this platform")
}
