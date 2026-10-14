//go:build !unix

package hostledger

import "errors"

// ErrLocked is TryLockFile's answer when another process holds the lock.
var ErrLocked = errors.New("locked by another process")

// LockFile is a no-op where the platform has no flock: lever's host side
// runs on macOS and Linux.
func LockFile(string) (func(), error) { return func() {}, nil }

// TryLockFile is a no-op like LockFile.
func TryLockFile(string) (func(), error) { return func() {}, nil }
