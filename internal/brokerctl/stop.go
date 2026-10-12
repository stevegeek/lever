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

// stopPollAttempts × stopPollInterval bounds each of the two waits in
// stopProcessByPIDFile (after SIGTERM, after SIGKILL) at ~2s.
const (
	stopPollAttempts = 40
	stopPollInterval = 50 * time.Millisecond
)

// stopProcessByPIDFile stops the process recorded in pidPath: SIGTERM (a
// graceful shutdown), wait up to ~2s for it to exit, SIGKILL a survivor, then
// poll briefly to confirm it actually died before returning — a caller that
// immediately re-spawns the same daemon (e.g. `lever reload`) would otherwise
// race the SIGKILLed process's release of its listen port and hit EADDRINUSE.
// Idempotent: a missing, unparseable, or already-dead pid file is a no-op.
//
// identify says what the pid is now (state.State.Identify). A pid file can
// outlive its process and the OS can give the pid to another one, which
// signal 0 cannot tell apart; so nothing is signalled unless the pid is the
// daemon just before the signal, the SIGKILL included. A pid that is not
// the daemon is a stale file: it is removed and nothing is signalled. A
// live pid whose argv cannot be read is neither: the file is kept, nothing
// is signalled, and the stop fails saying so. The wait ends when the pid
// is no longer the daemon (gone, a zombie, or reused).
//
// The process handle is taken BEFORE the check: on Linux os.FindProcess
// holds a pidfd, and the signals go through it, so a pid reused after the
// check gets nothing (the pidfd names the checked process, and signalling
// it once gone fails). Elsewhere a window of the check's duration remains.
//
// Shared by StopBroker (broker.pid) and StopRemoteProxy (remote.pid): both
// daemons are spawned the same Setsid way (see internal/cli/apply.go's
// brokerServeCmd/remoteServeCmd) and torn down identically.
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

// StopBroker stops the broker process recorded in the state dir's pid file
// and removes the pid file — see stopProcessByPIDFile for the exact
// mechanism. Tearing the broker down here is what keeps a stale broker — and
// its already-consumed single-use bootstrap latch — from poisoning the next
// `lever apply` (which would otherwise reuse it and stage no bootstrap
// ticket).
//
// In an instance directory copied or moved from another (lever#181), the
// pid file came with it and names the other directory's broker: it is
// removed, never signalled.
func StopBroker(st state.State) error {
	return stopOwnPIDFile(st, st.PID(), state.BrokerDaemon)
}

// stopOwnPIDFile is stopProcessByPIDFile for the `lever <sub> serve`
// daemon of this state directory, except that a pid file this state
// directory inherited from another (state.FromAnotherDir) is only removed.
func stopOwnPIDFile(st state.State, pidPath, sub string) error {
	if st.FromAnotherDir() {
		if err := os.Remove(pidPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("brokerctl: %w", err)
		}
		return nil
	}
	return stopProcessByPIDFile(pidPath, func(pid int) state.Identity { return st.Identify(pid, sub) })
}

// StopRemoteProxy stops the remote-access proxy process recorded in the state
// dir's remote.pid file — see stopProcessByPIDFile for the exact mechanism,
// identical to StopBroker. Called from `lever stop` (alongside StopBroker)
// and from apply.Run itself when the config disables remote access on an
// instance that has a stale proxy running (see apply.Deps.StopRemoteProxy) —
// both call sites may run against an already-stopped proxy, so idempotence
// matters here as much as it does for StopBroker.
func StopRemoteProxy(st state.State) error {
	return stopOwnPIDFile(st, st.RemotePID(), state.RemoteDaemon)
}
