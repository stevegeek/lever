package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// processArgs reads /proc. A process that is gone, a zombie and a kernel
// thread (empty cmdline) are errNotAProcess; any other read failure makes
// Identify say Unknown.
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
