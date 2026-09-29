//go:build !unix

package hostledger

// LockFile is a no-op where the platform has no flock: lever's host side
// runs on macOS and Linux.
func LockFile(string) (func(), error) { return func() {}, nil }
