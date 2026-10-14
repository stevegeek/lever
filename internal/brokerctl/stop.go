package brokerctl

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"

	"github.com/stevegeek/lever/internal/retry"
	"github.com/stevegeek/lever/internal/state"
)

const (
	stopPollAttempts = 40
	stopPollInterval = 50 * time.Millisecond
)

// stopProcessByPIDFile stops the process recorded in pidPath: SIGTERM, wait
// up to ~2s, SIGKILL a survivor, then confirm it died, or a caller that
// re-spawns the daemon at once (`lever reload`) races the port release and
// hits EADDRINUSE. A missing, unparseable or dead pid file is a no-op.
//
// identify says what the pid is now. A pid file can outlive its process and
// the pid can be reused, which signal 0 cannot tell, so nothing is signalled
// unless the pid is the daemon just before each signal. A pid that is not
// the daemon is a stale file: removed, nothing signalled. A live pid whose
// argv cannot be read keeps its file, gets no signal, and the stop fails.
//
// The process handle is taken before the check: on Linux os.FindProcess
// holds a pidfd and the signals go through it, so a pid reused after the
// check gets nothing. Elsewhere a window of the check's duration remains.
func stopProcessByPIDFile(pidPath string, identify func(pid int) state.Identity) error {
	pid, err := state.ReadPID(pidPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if errors.Is(err, state.ErrNotAPID) {
		_ = os.Remove(pidPath) // garbage pid file — clear it
		return nil
	}
	if err != nil {
		return fmt.Errorf("brokerctl: %w", err)
	}
	proc, ferr := os.FindProcess(pid)
	if ferr != nil {
		_ = os.Remove(pidPath)
		return nil
	}
	defer proc.Release()
	switch identify(pid) {
	case state.NotServe:
		_ = os.Remove(pidPath) // gone, or the pid is another process's now
		return nil
	case state.Unknown:
		return fmt.Errorf("brokerctl: %s names pid %d, a live process whose arguments cannot be read, so lever cannot tell whether it is its daemon: "+
			"nothing was signalled and the file is kept; stop that process by hand if it is, then remove the file", pidPath, pid)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		_ = os.Remove(pidPath) // already gone (ESRCH)
		return nil
	}
	gone := func() (bool, error) { return identify(pid) != state.IsServe, nil }
	if retry.Until(context.Background(), stopPollAttempts, stopPollInterval, gone) != nil && identify(pid) == state.IsServe {
		_ = proc.Signal(syscall.SIGKILL) // graceless fallback
		_ = retry.Until(context.Background(), stopPollAttempts, stopPollInterval, gone)
	}
	_ = os.Remove(pidPath)
	return nil
}

// StopBroker stops the broker recorded in the state dir's pid file. A stale
// broker, with its consumed single-use bootstrap latch, would otherwise be
// reused by the next apply, which would stage no bootstrap ticket. A pid
// file inherited from another instance directory is removed, never
// signalled.
func StopBroker(st state.State) error {
	return stopOwnPIDFile(st, st.PID(), state.BrokerDaemon)
}

// stopOwnPIDFile only removes a pid file inherited from another state
// directory (state.FromAnotherDir).
func stopOwnPIDFile(st state.State, pidPath, sub string) error {
	if st.FromAnotherDir() {
		if err := os.Remove(pidPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("brokerctl: %w", err)
		}
		return nil
	}
	return stopProcessByPIDFile(pidPath, func(pid int) state.Identity { return st.Identify(pid, sub) })
}

// StopRemoteProxy stops the remote-access proxy recorded in remote.pid, as
// StopBroker does. Its callers (`lever stop`, and apply when remote access
// is turned off) may run against an already-stopped proxy.
func StopRemoteProxy(st state.State) error {
	return stopOwnPIDFile(st, st.RemotePID(), state.RemoteDaemon)
}
