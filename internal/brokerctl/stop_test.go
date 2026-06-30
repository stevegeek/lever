package brokerctl

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestStopBrokerNoPidFileIsNoop(t *testing.T) {
	s := State{Dir: t.TempDir()}
	if err := s.StopBroker(); err != nil {
		t.Fatalf("StopBroker with no pid file must be a no-op, got %v", err)
	}
}

func TestStopBrokerKillsProcessAndRemovesPidFile(t *testing.T) {
	dir := t.TempDir()
	s := State{Dir: dir}

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

	if err := s.StopBroker(); err != nil {
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
	s := State{Dir: dir}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.PID(), []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.StopBroker(); err != nil {
		t.Fatalf("StopBroker with garbage pid must not error, got %v", err)
	}
	if _, err := os.Stat(s.PID()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("garbage pid file should be cleared")
	}
}
