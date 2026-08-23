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

func TestStopBrokerNoPidFileIsNoop(t *testing.T) {
	s := state.State{Dir: t.TempDir()}
	if err := StopBroker(s); err != nil {
		t.Fatalf("StopBroker with no pid file must be a no-op, got %v", err)
	}
}

func TestStopBrokerKillsProcessAndRemovesPidFile(t *testing.T) {
	dir := t.TempDir()
	s := state.State{Dir: dir}

	// A real long-lived child stands in for the broker process.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.PID(), []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := StopBroker(s); err != nil {
		t.Fatalf("StopBroker: %v", err)
	}

	// The pid file is removed.
	if _, err := os.Stat(s.PID()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pid file should be removed, stat err = %v", err)
	}

	// The process is gone. Reap it, then confirm signalling fails.
	_ = cmd.Wait()
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Fatal("process should have been killed")
	}
}

func TestStopBrokerGarbagePidFileCleared(t *testing.T) {
	dir := t.TempDir()
	s := state.State{Dir: dir}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.PID(), []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := StopBroker(s); err != nil {
		t.Fatalf("StopBroker with garbage pid must not error, got %v", err)
	}
	if _, err := os.Stat(s.PID()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("garbage pid file should be cleared")
	}
}

func TestStopRemoteProxyNoPidFileIsNoop(t *testing.T) {
	s := state.State{Dir: t.TempDir()}
	if err := StopRemoteProxy(s); err != nil {
		t.Fatalf("StopRemoteProxy with no pid file must be a no-op, got %v", err)
	}
}

// TestStopRemoteProxyKillsProcessAndRemovesPidFile mirrors
// TestStopBrokerKillsProcessAndRemovesPidFile exactly, against remote.pid
// instead of broker.pid — proving StopRemoteProxy shares stopProcessByPIDFile's
// real kill mechanism, not just its no-op paths.
func TestStopRemoteProxyKillsProcessAndRemovesPidFile(t *testing.T) {
	dir := t.TempDir()
	s := state.State{Dir: dir}

	// A real long-lived child stands in for the remote proxy process.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.RemotePID(), []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := StopRemoteProxy(s); err != nil {
		t.Fatalf("StopRemoteProxy: %v", err)
	}

	if _, err := os.Stat(s.RemotePID()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pid file should be removed, stat err = %v", err)
	}

	_ = cmd.Wait()
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Fatal("process should have been killed")
	}
}

func TestStopRemoteProxyGarbagePidFileCleared(t *testing.T) {
	dir := t.TempDir()
	s := state.State{Dir: dir}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.RemotePID(), []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := StopRemoteProxy(s); err != nil {
		t.Fatalf("StopRemoteProxy with garbage pid must not error, got %v", err)
	}
	if _, err := os.Stat(s.RemotePID()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("garbage pid file should be cleared")
	}
}

// TestStopBrokerAndStopRemoteProxyAreIndependent proves the two pid files
// don't cross-contaminate: stopping one must not touch the other's process
// or pid file.
func TestStopBrokerAndStopRemoteProxyAreIndependent(t *testing.T) {
	dir := t.TempDir()
	s := state.State{Dir: dir}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	brokerCmd := exec.Command("sleep", "60")
	if err := brokerCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = brokerCmd.Process.Kill(); _ = brokerCmd.Wait() }()
	if err := os.WriteFile(s.PID(), []byte(fmt.Sprintf("%d\n", brokerCmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}

	remoteCmd := exec.Command("sleep", "60")
	if err := remoteCmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.RemotePID(), []byte(fmt.Sprintf("%d\n", remoteCmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := StopRemoteProxy(s); err != nil {
		t.Fatalf("StopRemoteProxy: %v", err)
	}
	_ = remoteCmd.Wait()
	if err := remoteCmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Fatal("remote proxy process should have been killed")
	}

	// The broker's pid file and process must be untouched.
	if _, err := os.Stat(s.PID()); err != nil {
		t.Fatalf("broker.pid must survive a StopRemoteProxy call, stat err = %v", err)
	}
	if err := brokerCmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("broker process must survive a StopRemoteProxy call: %v", err)
	}
}
