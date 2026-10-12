//go:build unix && !linux

package voice

import (
	"os/exec"
	"strconv"
	"strings"
)

// procCommandLine is pid's argv joined by spaces, as ps(1) shows it (macOS
// has no /proc). The absolute path: the broker starts a tool with a fixed
// PATH.
func procCommandLine(pid int) (string, error) {
	out, err := exec.Command("/bin/ps", "-ww", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
