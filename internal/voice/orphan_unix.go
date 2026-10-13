//go:build unix

package voice

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const orphanPortWait = 5 * time.Second

// writePidFile writes 0600 by rename, never following a link at path.
func writePidFile(path string, pid int) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".whisper-server.pid-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.WriteString(strconv.Itoa(pid) + "\n")
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, 0o600)
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name)
	}
	return werr
}

// readPidFile requires a small regular file (no link) holding one number
// above 1.
func readPidFile(path string) (int, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > 32 {
		return 0, fmt.Errorf("%s is not a pid file", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("%s does not hold a pid", path)
	}
	return pid, nil
}

// removePidFile leaves a file naming another pid (a newer child's) alone.
func removePidFile(path string, pid int) {
	if got, err := readPidFile(path); err == nil && got == pid {
		_ = os.Remove(path)
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// isOurServer reports whether pid's command line starts with the program and
// the host and port arguments ServerArgs writes. Nothing else is ever
// signalled: a pid file can outlive its process and the pid be reused.
func (s *Supervisor) isOurServer(pid int) bool {
	if s.Program == "" || !alive(pid) {
		return false
	}
	line, err := procCommandLine(pid)
	if err != nil {
		return false
	}
	want := s.Program + " --host 127.0.0.1 --port " + strconv.Itoa(s.Port)
	return line == want || strings.HasPrefix(line, want+" ")
}

// reapOrphan stops a whisper-server a killed tool left behind, only once
// isOurServer verifies it, process group first; then it waits for the port
// and removes the file. A file naming anything else is removed and nothing
// is signalled.
func (s *Supervisor) reapOrphan() {
	if s.PidFile == "" {
		return
	}
	pid, err := readPidFile(s.PidFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.logf("voice: ignoring the pid file: %v", err)
			_ = os.Remove(s.PidFile)
		}
		return
	}
	if !s.isOurServer(pid) {
		_ = os.Remove(s.PidFile)
		return
	}
	s.logf("voice: stopping whisper-server left running by an earlier tool (pid %d)", pid)
	signalTree(pid, syscall.SIGTERM)
	deadline := time.Now().Add(dur(s.StopGrace, defaultStopGrace))
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pid) && s.isOurServer(pid) {
		signalTree(pid, syscall.SIGKILL)
	}
	deadline = time.Now().Add(orphanPortWait)
	for s.portTaken() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	_ = os.Remove(s.PidFile)
}

// signalTree signals pid's process group when pid leads it, else pid alone.
func signalTree(pid int, sig syscall.Signal) {
	if pg, err := syscall.Getpgid(pid); err == nil && pg == pid {
		_ = syscall.Kill(-pid, sig)
		return
	}
	_ = syscall.Kill(pid, sig)
}
