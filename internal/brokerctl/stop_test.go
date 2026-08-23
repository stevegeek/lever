package brokerctl

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stevegeek/lever/internal/state"
)

// stopTarget is one pid-file-backed daemon: which pid file it reads and
// which stop function owns it. StopBroker and StopRemoteProxy share
// stopProcessByPIDFile, so each test body below runs against both.
type stopTarget struct {
	name    string
	pidFile func(state.State) string
	stop    func(state.State) error
}

var (
	brokerTarget = stopTarget{"StopBroker", state.State.PID, StopBroker}
	remoteTarget = stopTarget{"StopRemoteProxy", state.State.RemotePID, StopRemoteProxy}
)

// startSleeper starts a real long-lived child to stand in for a daemon
// process, writes its pid to path, and returns the command. The caller reaps it.
func startSleeper(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func newStopState(t *testing.T) state.State {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return state.State{Dir: dir}
}

// assertKilled reaps cmd and confirms it no longer accepts a signal.
func assertKilled(t *testing.T, cmd *exec.Cmd, what string) {
	t.Helper()
	_ = cmd.Wait()
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Fatalf("%s should have been killed", what)
	}
}

func assertPidFileGone(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s, stat err = %v", what, err)
	}
}

// testStopNoPidFileIsNoop, testStopKillsProcessAndRemovesPidFile and
// testStopGarbagePidFileCleared are the shared bodies; the named tests below
// run each against one target.
func testStopNoPidFileIsNoop(t *testing.T, tgt stopTarget) {
	s := state.State{Dir: t.TempDir()}
	if err := tgt.stop(s); err != nil {
		t.Fatalf("%s with no pid file must be a no-op, got %v", tgt.name, err)
	}
}

func testStopKillsProcessAndRemovesPidFile(t *testing.T, tgt stopTarget) {
	s := newStopState(t)
	cmd := startSleeper(t, tgt.pidFile(s))

	if err := tgt.stop(s); err != nil {
		t.Fatalf("%s: %v", tgt.name, err)
	}
	assertPidFileGone(t, tgt.pidFile(s), "pid file should be removed")
	assertKilled(t, cmd, "process")
}

func testStopGarbagePidFileCleared(t *testing.T, tgt stopTarget) {
	s := newStopState(t)
	if err := os.WriteFile(tgt.pidFile(s), []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tgt.stop(s); err != nil {
		t.Fatalf("%s with garbage pid must not error, got %v", tgt.name, err)
	}
	assertPidFileGone(t, tgt.pidFile(s), "garbage pid file should be cleared")
}

func TestStopBrokerNoPidFileIsNoop(t *testing.T) { testStopNoPidFileIsNoop(t, brokerTarget) }

func TestStopBrokerKillsProcessAndRemovesPidFile(t *testing.T) {
	testStopKillsProcessAndRemovesPidFile(t, brokerTarget)
}

func TestStopBrokerGarbagePidFileCleared(t *testing.T) {
	testStopGarbagePidFileCleared(t, brokerTarget)
}

func TestStopRemoteProxyNoPidFileIsNoop(t *testing.T) { testStopNoPidFileIsNoop(t, remoteTarget) }

// TestStopRemoteProxyKillsProcessAndRemovesPidFile mirrors
// TestStopBrokerKillsProcessAndRemovesPidFile exactly, against remote.pid
// instead of broker.pid — proving StopRemoteProxy shares stopProcessByPIDFile's
// real kill mechanism, not just its no-op paths.
func TestStopRemoteProxyKillsProcessAndRemovesPidFile(t *testing.T) {
	testStopKillsProcessAndRemovesPidFile(t, remoteTarget)
}

func TestStopRemoteProxyGarbagePidFileCleared(t *testing.T) {
	testStopGarbagePidFileCleared(t, remoteTarget)
}

// TestStopBrokerAndStopRemoteProxyAreIndependent proves the two pid files
// don't cross-contaminate: stopping one must not touch the other's process
// or pid file.
func TestStopBrokerAndStopRemoteProxyAreIndependent(t *testing.T) {
	s := newStopState(t)

	brokerCmd := startSleeper(t, s.PID())
	defer func() { _ = brokerCmd.Process.Kill(); _ = brokerCmd.Wait() }()
	remoteCmd := startSleeper(t, s.RemotePID())

	if err := StopRemoteProxy(s); err != nil {
		t.Fatalf("StopRemoteProxy: %v", err)
	}
	assertKilled(t, remoteCmd, "remote proxy process")

	// The broker's pid file and process must be untouched.
	if _, err := os.Stat(s.PID()); err != nil {
		t.Fatalf("broker.pid must survive a StopRemoteProxy call, stat err = %v", err)
	}
	if err := brokerCmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("broker process must survive a StopRemoteProxy call: %v", err)
	}
}
