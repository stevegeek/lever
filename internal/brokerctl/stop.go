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
// Note: the pid could in principle have been recycled by the OS after the
// original process died. On the single-operator workstation this targets, the
// window is small and the pid file is written immediately after spawn;
// callers that need certainty can additionally confirm the process is still
// listening before relying on it being gone.
//
// Shared by StopBroker (broker.pid) and StopRemoteProxy (remote.pid): both
// daemons are spawned the same Setsid way (see internal/cli/apply.go's
// brokerServeCmd/remoteServeCmd) and torn down identically.
func stopProcessByPIDFile(pidPath string) error {
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
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		_ = os.Remove(pidPath) // already gone (ESRCH)
		return nil
	}
	gone := func() (bool, error) { return !state.ProcessAlive(pid), nil }
	if retry.Until(context.Background(), stopPollAttempts, stopPollInterval, gone) != nil {
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
func StopBroker(st state.State) error {
	return stopProcessByPIDFile(st.PID())
}

// StopRemoteProxy stops the remote-access proxy process recorded in the state
// dir's remote.pid file — see stopProcessByPIDFile for the exact mechanism,
// identical to StopBroker. Called from `lever stop` (alongside StopBroker)
// and from apply.Run itself when the config disables remote access on an
// instance that has a stale proxy running (see apply.Deps.StopRemoteProxy) —
// both call sites may run against an already-stopped proxy, so idempotence
// matters here as much as it does for StopBroker.
func StopRemoteProxy(st state.State) error {
	return stopProcessByPIDFile(st.RemotePID())
}
