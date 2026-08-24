package state

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// ErrNotAPID is returned (wrapped) by ReadPID when the file exists but does
// not hold a positive integer.
var ErrNotAPID = errors.New("does not contain a pid")

// ReadPID parses the pid recorded in the file at path. A missing file returns
// the read error (os.ErrNotExist-wrapped); garbage or a non-positive number
// wraps ErrNotAPID.
func ReadPID(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("state: read pid file %s: %w", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("state: %s %w", path, ErrNotAPID)
	}
	return pid, nil
}

// ProcessAlive reports whether pid names a running process, by a signal-0
// probe. The pid could in principle have been recycled by the OS, but on the
// single-operator workstation this targets that window is small.
func ProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// PIDStatus reads the pid recorded at path and reports whether that process
// is currently alive. It is the read-only counterpart to stopping a daemon,
// used by `lever doctor` and `lever remote status` to tell "never started"
// from "died".
//
//   - found=false: no pid file (never started, or cleanly stopped).
//   - found=true, alive=false: a stale pid file — the process is gone (or the
//     file is garbage). This is the "died out from under apply" case.
//   - found=true, alive=true: the recorded process is running (pid is returned).
func PIDStatus(path string) (pid int, found, alive bool) {
	if _, err := os.Stat(path); err != nil {
		return 0, false, false
	}
	pid, err := ReadPID(path)
	if err != nil {
		return 0, true, false // garbage pid file: found, but not a live process
	}
	return pid, true, ProcessAlive(pid)
}
