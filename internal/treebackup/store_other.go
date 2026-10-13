//go:build !linux && !darwin

package treebackup

import (
	"io/fs"
	"os"
)

const platformSupported = false

const oNoFollow = 0

func syncStoreFS(*os.File) error                      { return errPlatform }
func syncNewBlob(*os.File) error                      { return errPlatform }
func syncManifest(*os.File) error                     { return errPlatform }
func fsync(*os.File) error                            { return errPlatform }
func preallocate(*os.File, int64) error               { return errPlatform }
func flockTry(*os.File, bool) error                   { return errPlatform }
func flockUnlock(*os.File) error                      { return errPlatform }
func fileKey(fs.FileInfo) (string, bool)              { return "", false }
func allocated(fs.FileInfo) int64                     { return 0 }
func fsFree(*os.File) (uint64, uint64, uint64, error) { return 0, 0, 0, errPlatform }

func isCopyOnWrite(*os.File) bool { return false }
