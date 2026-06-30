package brokerctl

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// StopBroker stops the broker process recorded in the state dir's pid file and
// removes the pid file. It sends SIGTERM (the broker shuts down gracefully),
// waits up to ~2s for it to exit, then SIGKILLs a survivor. It is idempotent: a
// missing or unparseable pid file is a no-op (the broker was never started, or
// already stopped). Tearing the broker down here is what keeps a stale broker —
// and its already-consumed single-use bootstrap latch — from poisoning the next
// `lever apply` (which would otherwise reuse it and stage no bootstrap ticket).
//
// Note: the pid could in principle have been recycled by the OS after the broker
// died. On the single-operator workstation this targets, the window is small and
// the pid file is written immediately after spawn; callers that need certainty
// can additionally confirm the admin port before relying on the broker being gone.
func (s State) StopBroker() error {
	pidPath := s.PID()
	data, err := os.ReadFile(pidPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("brokerctl: read broker pid: %w", err)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(data)))
	if perr != nil || pid <= 0 {
		_ = os.Remove(pidPath) // garbage pid file — clear it
		return nil
	}
	proc, ferr := os.FindProcess(pid)
	if ferr != nil {
		_ = os.Remove(pidPath)
		return nil
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		_ = os.Remove(pidPath) // already gone (ESRCH)
		return nil
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if proc.Signal(syscall.Signal(0)) != nil {
			_ = os.Remove(pidPath) // exited
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = proc.Signal(syscall.SIGKILL) // graceless fallback
	_ = os.Remove(pidPath)
	return nil
}
