//go:build !linux && !darwin

package voice

import "io/fs"

// fileStamp: no description here, so every check verifies the files whole.
func fileStamp(fs.FileInfo) string { return "" }
