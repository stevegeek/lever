package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// processArgs is pid's argv, from /proc. A process that is gone, a zombie
// and a kernel thread (whose cmdline is empty) are errNotAProcess; any
// other read failure is errNoArgs-like (Identify says Unknown).
func processArgs(pid int) ([]string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNotAProcess
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoArgs, err)
	}
	s := strings.TrimSuffix(string(b), "\x00")
	if s == "" {
		return nil, errNotAProcess
	}
	return strings.Split(s, "\x00"), nil
}
