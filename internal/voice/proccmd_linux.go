//go:build linux

package voice

import (
	"fmt"
	"os"
	"strings"
)

// procCommandLine is pid's argv joined by spaces, from /proc.
func procCommandLine(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(strings.TrimRight(string(b), "\x00"), "\x00", " "), nil
}
