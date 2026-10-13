//go:build unix

package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// StartServeProcess starts a real, long-lived process whose argv is
// `lever <sub> serve <configPath>`, the shape state.ServeProcess recognises
// as a lever daemon (an empty configPath omits the argument). It is
// /bin/sh running a script file named sub in a temp dir, so the argv is
// exactly that and the process sleeps until stopped. The process gets its
// own process group, and the cleanup kills the group. It is reaped as soon
// as it exits; done is closed then.
func StartServeProcess(t testing.TB, sub, configPath string) (cmd *exec.Cmd, done <-chan struct{}) {
	t.Helper()
	return StartServeProcessScript(t, sub, configPath, "")
}

// StartServeProcessScript is StartServeProcess with prelude (sh commands,
// e.g. a trap) run before the sleep.
func StartServeProcessScript(t testing.TB, sub, configPath, prelude string) (cmd *exec.Cmd, done <-chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	// The ready file says the script runs, so the kernel shows its argv
	// (right after exec there is a moment where it does not yet). The
	// trailing ":" keeps the shell from exec'ing sleep in its place.
	if err := os.WriteFile(filepath.Join(dir, sub), []byte(prelude+"\n: > ready\nsleep 60\n:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"lever", sub, "serve"}
	if configPath != "" {
		args = append(args, configPath)
	}
	cmd = &exec.Cmd{Path: "/bin/sh", Args: args, Dir: dir, SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
	})
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %s serve stand-in never started", sub)
		}
	}
	return cmd, exited
}
