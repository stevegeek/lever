//go:build linux

package voice

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// listenerOwnedBy reports whether the TCP listener on 127.0.0.1:port is a
// socket of process pid, from /proc. known is false when /proc cannot
// tell (then the caller trusts the port, as off Linux).
//
// whisper-server loads its model before it binds, which takes seconds; in
// that window another local process could bind the port, and the readiness
// probe would connect to it. This check keeps lever from sending audio to
// anything but its own child.
func listenerOwnedBy(pid, port int) (owned, known bool) {
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return false, false
	}
	want := fmt.Sprintf("0100007F:%04X", port) // 127.0.0.1 (little-endian), the port in hex
	inode := ""
	for _, line := range strings.Split(string(data), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) > 9 && f[1] == want && f[3] == "0A" { // 0A: LISTEN
			inode = f[9]
			break
		}
	}
	if inode == "" {
		return false, true
	}
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, false
	}
	for _, e := range entries {
		if t, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil && t == "socket:["+inode+"]" {
			return true, true
		}
	}
	return false, true
}
