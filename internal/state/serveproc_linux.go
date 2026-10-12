package state

import (
	"fmt"
	"os"
	"strings"
)

// processArgs is pid's argv, from /proc. A zombie's cmdline is empty,
// which reads as no argv.
func processArgs(pid int) ([]string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	s := strings.TrimSuffix(string(b), "\x00")
	if s == "" {
		return nil, errNoArgs
	}
	return strings.Split(s, "\x00"), nil
}
