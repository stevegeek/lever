//go:build !linux && !darwin

package treebackup

import (
	"fmt"
	"runtime"
)

func newSysFS() (restoreFS, error) {
	return nil, fmt.Errorf("tree restore: not supported on %s", runtime.GOOS)
}
