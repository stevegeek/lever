//go:build !linux && !darwin

package fsutil

import "errors"

// InsideByIdentity reports whether inner is outer or lies below it. It is
// implemented on Linux and macOS only; elsewhere it always fails, and
// callers treat that as "inside".
func InsideByIdentity(inner, outer string) (bool, error) {
	return false, errors.New("identity checks are not supported on this platform")
}
