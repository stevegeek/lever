//go:build !unix

package chatledger

import "io/fs"

func fileOwner(fs.FileInfo) (int, bool) { return 0, false }

const oNoFollow = 0
