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

// ReadPID parses the pid in the file at path. A missing file returns the
// read error; garbage or a non-positive number wraps ErrNotAPID.
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

// ProcessAlive probes pid with signal 0. A recycled pid could fool it; on a
// single-operator workstation that window is small.
func ProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// PIDStatus reads the pid at path and reports whether it is alive, so doctor
// and `lever remote status` can tell "never started" (found=false) from
// "died" (found, not alive; also a garbage file).
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
