//go:build !unix

package voice

import (
	"os"
	"syscall"
)

// PidFileName: see orphan_unix.go. Off unix no pid file is kept.
const PidFileName = "whisper-server.pid"

func writePidFile(string, int) error { return nil }
func removePidFile(string, int)      {}
func (s *Supervisor) reapOrphan()    {}

// signalTree signals the child alone.
func signalTree(pid int, sig syscall.Signal) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(sig)
	}
}
