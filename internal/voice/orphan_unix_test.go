//go:build unix

package voice

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// startOrphan starts bin as a whisper-server a killed tool left behind:
// in its own process group, not a child the supervisor knows.
func startOrphan(t *testing.T, bin string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	})
	return cmd
}

func gone(cmd *exec.Cmd) bool {
	// The test's Wait goroutine reaps it, after which signal 0 fails.
	return syscall.Kill(cmd.Process.Pid, 0) != nil
}

func TestPidFileWriteReadRemove(t *testing.T) {
	p := filepath.Join(t.TempDir(), PidFileName(8448))
	if err := writePidFile(p, 4242); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v %v, want 0600", fi.Mode(), err)
	}
	if pid, err := readPidFile(p); err != nil || pid != 4242 {
		t.Fatalf("read = %d %v", pid, err)
	}
	removePidFile(p, 1111) // another child's: kept
	if _, err := os.Lstat(p); err != nil {
		t.Fatal("a file naming another pid must be kept")
	}
	removePidFile(p, 4242)
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatal("the file must be removed")
	}
	for _, bad := range []string{"", "x", "1", "-5", "0"} {
		if err := os.WriteFile(p, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readPidFile(p); err == nil {
			t.Errorf("%q read as a pid", bad)
		}
	}
	os.Remove(p)
	if err := os.Symlink("/dev/null", p); err != nil {
		t.Fatal(err)
	}
	if _, err := readPidFile(p); err == nil {
		t.Error("a link read as a pid file")
	}
}

// The orphan the pid file names is the configured program on our port: it
// is stopped, the port frees, the file goes, and the child starts.
func TestSupervisorReapsItsOrphan(t *testing.T) {
	bin := fakeWhisper(t)
	port := freePort(t)
	model := modelFile(t)
	args := ServerArgs(model, port, false, "/old")
	orphan := startOrphan(t, bin, args...)
	eventually(t, "the orphan listening", func() bool { return listening(port) })
	pidFile := filepath.Join(t.TempDir(), PidFileName(8448))
	if err := writePidFile(pidFile, orphan.Process.Pid); err != nil {
		t.Fatal(err)
	}
	var l logs
	s := &Supervisor{Program: bin, Args: ServerArgs(model, port, false, "/secret"), Port: port, PidFile: pidFile, Log: l.f,
		MinBackoff: 20 * time.Millisecond, ProbeEvery: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	eventually(t, "ready after the reap", s.Ready)
	if !gone(orphan) || l.count("left running by an earlier tool") != 1 || l.count("already in use") != 0 {
		t.Fatalf("orphan gone=%v logs=%q", gone(orphan), l.lines)
	}
	// The new child is in the file now, in its own process group.
	pid, err := readPidFile(pidFile)
	if err != nil || pid == orphan.Process.Pid {
		t.Fatalf("pid file = %d %v, want the new child", pid, err)
	}
	if fi, err := os.Stat(pidFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("pid file mode = %v %v", fi, err)
	}
	if pg, err := syscall.Getpgid(pid); err != nil || pg != pid {
		t.Fatalf("child pgid = %d %v, want its own group %d", pg, err, pid)
	}
	cancel()
	<-done
	if _, err := os.Lstat(pidFile); !os.IsNotExist(err) {
		t.Fatal("the pid file must go with the child")
	}
	if listening(port) {
		t.Fatal("the child outlived Run")
	}
}

// A pid file that names anything else (another program, our program on
// another port, a dead pid) is removed and nothing is signalled.
func TestSupervisorNeverKillsAnUnrelatedPid(t *testing.T) {
	bin := fakeWhisper(t)
	port := freePort(t)
	model := modelFile(t)
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep(1)")
	}
	other := freePort(t)
	for name, cmd := range map[string]*exec.Cmd{
		"another program":         startOrphan(t, sleep, "30"),
		"our program, other port": startOrphan(t, bin, ServerArgs(model, other, false, "/x")...),
	} {
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), PidFileName(8448))
			if err := writePidFile(pidFile, cmd.Process.Pid); err != nil {
				t.Fatal(err)
			}
			s := &Supervisor{Program: bin, Port: port, PidFile: pidFile}
			s.reapOrphan()
			time.Sleep(100 * time.Millisecond)
			if gone(cmd) {
				t.Fatal("an unrelated process was signalled")
			}
			if _, err := os.Lstat(pidFile); !os.IsNotExist(err) {
				t.Fatal("the stale pid file must be removed")
			}
		})
	}
	// A dead pid: the file goes.
	dead := exec.Command(sleep, "0")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), PidFileName(8448))
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(dead.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	(&Supervisor{Program: bin, Port: port, PidFile: pidFile}).reapOrphan()
	if _, err := os.Lstat(pidFile); !os.IsNotExist(err) {
		t.Fatal("a dead pid's file must be removed")
	}
}
