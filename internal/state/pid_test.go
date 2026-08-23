package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeTestPIDFile writes an arbitrary pid-file body directly (bypassing
// daemon.WritePIDFile) so these tests can exercise PIDStatus against
// live/stale/garbage contents without spawning a process.
func writeTestPIDFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.pid")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadPID(t *testing.T) {
	if pid, err := ReadPID(writeTestPIDFile(t, " 42\n")); err != nil || pid != 42 {
		t.Fatalf("ReadPID = %d, %v; want 42", pid, err)
	}
	for _, body := range []string{"", "abc", "0", "-3"} {
		if _, err := ReadPID(writeTestPIDFile(t, body)); !errors.Is(err, ErrNotAPID) {
			t.Errorf("ReadPID(%q): err = %v, want ErrNotAPID", body, err)
		}
	}
	_, err := ReadPID(filepath.Join(t.TempDir(), "missing.pid"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: err = %v, want ErrNotExist", err)
	}
}

func TestProcessAlive(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Fatal("own pid must be alive")
	}
	if ProcessAlive(2147483646) { // implausibly high pid => no such process
		t.Fatal("implausible pid must not be alive")
	}
}

func TestPIDStatusNoFile(t *testing.T) {
	_, found, alive := PIDStatus(filepath.Join(t.TempDir(), "none.pid"))
	if found || alive {
		t.Fatalf("no pid file => found=false alive=false; got found=%v alive=%v", found, alive)
	}
}

func TestPIDStatusLiveSelf(t *testing.T) {
	pid, found, alive := PIDStatus(writeTestPIDFile(t, strconv.Itoa(os.Getpid())+"\n"))
	if !found || !alive || pid != os.Getpid() {
		t.Fatalf("own pid is alive; got pid=%d found=%v alive=%v", pid, found, alive)
	}
}

func TestPIDStatusStale(t *testing.T) {
	pid, found, alive := PIDStatus(writeTestPIDFile(t, "2147483646\n"))
	if !found || alive {
		t.Fatalf("stale pid => found=true alive=false; got pid=%d found=%v alive=%v", pid, found, alive)
	}
}

func TestPIDStatusGarbage(t *testing.T) {
	_, found, alive := PIDStatus(writeTestPIDFile(t, "not-a-pid\n"))
	if !found || alive {
		t.Fatalf("garbage pid file => found=true alive=false; got found=%v alive=%v", found, alive)
	}
}
