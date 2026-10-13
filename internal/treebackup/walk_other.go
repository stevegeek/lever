//go:build !linux && !darwin

package treebackup

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"time"
)

const platformChecksDev = true

var listingWindow = time.Second

var errWalkPlatform = fmt.Errorf("%w: %s", ErrUnsupported, runtime.GOOS)

func sysProbeKernel() error                          { return errWalkPlatform }
func sysOpenRoot(string) (int, error)                { return -1, errWalkPlatform }
func sysProbeRoot(int) error                         { return errWalkPlatform }
func sysOpenDir(int, string) (int, error)            { return -1, errWalkPlatform }
func sysOpenListing(int) (int, error)                { return -1, errWalkPlatform }
func sysOpenFile(int, string) (int, error)           { return -1, errWalkPlatform }
func sysFstatat(int, string) (sysStat, error)        { return sysStat{}, errWalkPlatform }
func sysFstat(int) (sysStat, error)                  { return sysStat{}, errWalkPlatform }
func sysLstat(string) (sysStat, error)               { return sysStat{}, errWalkPlatform }
func sysReadlinkat(int, string, []byte) (int, error) { return 0, errWalkPlatform }
func sysPread(int, []byte, int64) (int, error)       { return 0, errWalkPlatform }
func sysClose(int)                                   {}
func classify(error) errClass                        { return errOther }
func isNoSpace(err error) bool                       { return errors.Is(err, syscall.ENOSPC) }
