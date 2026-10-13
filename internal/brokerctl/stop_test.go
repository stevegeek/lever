package brokerctl

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/testutil"
)

// stopTarget is one pid-file-backed daemon: which pid file it reads and
// which stop function owns it. StopBroker and StopRemoteProxy share
// stopProcessByPIDFile, so each test body below runs against both.
type stopTarget struct {
	name    string
	pidFile func(state.State) string
	stop    func(state.State) error
	sub     string // the daemon's `lever <sub> serve`
}

var (
	brokerTarget = stopTarget{"StopBroker", state.State.PID, StopBroker, state.BrokerDaemon}
	remoteTarget = stopTarget{"StopRemoteProxy", state.State.RemotePID, StopRemoteProxy, state.RemoteDaemon}
)

// sleeper is a real long-lived child standing in for a daemon process.
type sleeper struct {
	cmd  *exec.Cmd
	done <-chan struct{} // closed once it has exited and been reaped
}

// startSleeper starts a stand-in for tgt's daemon of s (its argv is
// `lever <broker|remote> serve <config beside s>`, what
// state.ServeProcess recognises) and writes its pid to tgt's pid file.
func startSleeper(t *testing.T, s state.State, tgt stopTarget) sleeper {
	t.Helper()
	cmd, done := testutil.StartServeProcess(t, tgt.sub, filepath.Join(filepath.Dir(s.Dir), "lever.yaml"))
	writePID(t, tgt.pidFile(s), cmd.Process.Pid)
	return sleeper{cmd, done}
}

func writePID(t *testing.T, path string, pid int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", pid)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newStopState is the state dir of an instance whose config is in a temp
// dir, as state.ForConfig makes it.
func newStopState(t *testing.T) state.State {
	t.Helper()
	s := state.ForConfig(t.TempDir())
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return s
}

// assertKilled waits for p to exit (it is reaped as it does).
func assertKilled(t *testing.T, p sleeper, what string) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s should have been killed", what)
	}
}

// assertAlive fails unless p is still running.
func assertAlive(t *testing.T, p sleeper, what string) {
	t.Helper()
	select {
	case <-p.done:
		t.Fatalf("%s must not have been signalled", what)
	default:
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
	cmd := startSleeper(t, s, tgt)

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

	brokerCmd := startSleeper(t, s, brokerTarget)
	remoteCmd := startSleeper(t, s, remoteTarget)

	if err := StopRemoteProxy(s); err != nil {
		t.Fatalf("StopRemoteProxy: %v", err)
	}
	assertKilled(t, remoteCmd, "remote proxy process")

	// The broker's pid file and process must be untouched.
	if _, err := os.Stat(s.PID()); err != nil {
		t.Fatalf("broker.pid must survive a StopRemoteProxy call, stat err = %v", err)
	}
	assertAlive(t, brokerCmd, "the broker process (by StopRemoteProxy)")
}

// copyIDFrom gives s the instance id file of another state dir, as a copy
// of the instance directory would (lever#181).
func copyIDFrom(t *testing.T, s state.State) {
	t.Helper()
	orig := state.State{Dir: t.TempDir()}
	if _, err := orig.EnsureInstanceID(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(orig.InstanceIDFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.InstanceIDFile(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// In a copied instance directory the pid file names the original's
// process: the stop removes the file and never signals the process.
func TestStopLeavesACopiedPidFilesProcessAlone(t *testing.T) {
	for _, tgt := range []stopTarget{brokerTarget, remoteTarget} {
		t.Run(tgt.name, func(t *testing.T) {
			s := newStopState(t)
			copyIDFrom(t, s)
			cmd := startSleeper(t, s, tgt)
			if err := tgt.stop(s); err != nil {
				t.Fatalf("%s: %v", tgt.name, err)
			}
			assertPidFileGone(t, tgt.pidFile(s), "the copied pid file should be removed")
			assertAlive(t, cmd, tgt.name+": the original's process")
		})
	}
}

// A pid file whose pid the OS gave to another process (here, one that is
// not a lever daemon, and one that is the other daemon, and one that is
// another instance's daemon) is stale: the stop removes the file and
// signals nothing.
func TestStopLeavesAReusedPidAlone(t *testing.T) {
	for _, tgt := range []stopTarget{brokerTarget, remoteTarget} {
		t.Run(tgt.name, func(t *testing.T) {
			s := newStopState(t)
			other := newStopState(t)
			unrelated, unrelatedDone := testutil.StartServeProcess(t, "notlever", "")
			wrongDaemon := brokerTarget
			if tgt.sub == brokerTarget.sub {
				wrongDaemon = remoteTarget
			}
			for name, p := range map[string]sleeper{
				"an unrelated process":      {unrelated, unrelatedDone},
				"the other daemon":          startSleeperFor(t, s, wrongDaemon.sub),
				"another instance's daemon": startSleeperFor(t, other, tgt.sub),
			} {
				writePID(t, tgt.pidFile(s), p.cmd.Process.Pid)
				if err := tgt.stop(s); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				assertPidFileGone(t, tgt.pidFile(s), name+": the stale pid file should be removed")
				assertAlive(t, p, name)
			}
		})
	}
}

// startSleeperFor starts a stand-in `lever <sub> serve` for s without
// writing any pid file.
func startSleeperFor(t *testing.T, s state.State, sub string) sleeper {
	t.Helper()
	cmd, done := testutil.StartServeProcess(t, sub, filepath.Join(filepath.Dir(s.Dir), "lever.yaml"))
	return sleeper{cmd, done}
}

// The guard is real: this test process is alive but is no daemon, so a
// pid file naming it is removed and the test survives.
func TestStopNeverSignalsThisProcess(t *testing.T) {
	for _, tgt := range []stopTarget{brokerTarget, remoteTarget} {
		s := newStopState(t)
		writePID(t, tgt.pidFile(s), os.Getpid())
		if err := tgt.stop(s); err != nil {
			t.Fatal(err)
		}
		assertPidFileGone(t, tgt.pidFile(s), "stale")
	}
}

// A daemon that ignores SIGTERM is SIGKILLed once the wait runs out (it is
// still the daemon then, checked again before the SIGKILL).
func TestStopKillsADaemonThatIgnoresSIGTERM(t *testing.T) {
	s := newStopState(t)
	cmd, done := testutil.StartServeProcessScript(t, state.BrokerDaemon, filepath.Join(filepath.Dir(s.Dir), "lever.yaml"), "trap '' TERM")
	writePID(t, s.PID(), cmd.Process.Pid)
	if err := StopBroker(s); err != nil {
		t.Fatal(err)
	}
	assertKilled(t, sleeper{cmd, done}, "a daemon ignoring SIGTERM")
	assertPidFileGone(t, s.PID(), "the pid file should be removed")
}

// A live pid whose identity cannot be read is neither signalled nor
// forgotten: the stop fails and keeps the file.
func TestStopKeepsAPidItCannotIdentify(t *testing.T) {
	s := newStopState(t)
	p := startSleeper(t, s, brokerTarget)
	err := stopProcessByPIDFile(s.PID(), func(int) state.Identity { return state.Unknown })
	if err == nil {
		t.Fatal("an unidentifiable pid was not reported")
	}
	if _, serr := os.Stat(s.PID()); serr != nil {
		t.Fatalf("the pid file was removed: %v", serr)
	}
	assertAlive(t, p, "an unidentifiable process")
}
