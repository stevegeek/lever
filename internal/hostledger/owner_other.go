//go:build !unix

package hostledger

import "io/fs"

// FileOwner is the uid that owns fi, where the platform reports one.
func FileOwner(fs.FileInfo) (int, bool) { return 0, false }

const ONoFollow = 0
