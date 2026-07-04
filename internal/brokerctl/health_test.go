package brokerctl

import (
	"os"
	"strconv"
	"testing"
)

// writeTestPIDFile writes an arbitrary pid-file body directly (bypassing the
// real writePIDFile in serve.go) so these tests can exercise BrokerPIDStatus
// against live/stale/garbage contents without spawning a process.
func writeTestPIDFile(t *testing.T, s State, body string) {
	t.Helper()
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.PID(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBrokerPIDStatusNoFile(t *testing.T) {
	_, found, alive := StateDir(t.TempDir()).BrokerPIDStatus()
	if found || alive {
		t.Fatalf("no pid file => found=false alive=false; got found=%v alive=%v", found, alive)
	}
}

func TestBrokerPIDStatusLiveSelf(t *testing.T) {
	s := StateDir(t.TempDir())
	writeTestPIDFile(t, s, strconv.Itoa(os.Getpid())+"\n")
	pid, found, alive := s.BrokerPIDStatus()
	if !found || !alive || pid != os.Getpid() {
		t.Fatalf("own pid is alive; got pid=%d found=%v alive=%v", pid, found, alive)
	}
}

func TestBrokerPIDStatusStale(t *testing.T) {
	s := StateDir(t.TempDir())
	writeTestPIDFile(t, s, "2147483646\n") // implausibly high pid => no such process
	pid, found, alive := s.BrokerPIDStatus()
	if !found || alive {
		t.Fatalf("stale pid => found=true alive=false; got pid=%d found=%v alive=%v", pid, found, alive)
	}
}

func TestBrokerPIDStatusGarbage(t *testing.T) {
	s := StateDir(t.TempDir())
	writeTestPIDFile(t, s, "not-a-pid\n")
	_, found, alive := s.BrokerPIDStatus()
	if !found || alive {
		t.Fatalf("garbage pid file => found=true alive=false; got found=%v alive=%v", found, alive)
	}
}
