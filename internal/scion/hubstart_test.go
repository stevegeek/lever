package scion

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion/layout"
)

// hubStartRunner models the guest around scion server start: each start
// answers from starts in turn; after it, the pid probe reports a live server
// when liveAfter says so, and scion list answers when the hub serves.
type hubStartRunner struct {
	starts    []string // "" succeeds; otherwise scion's output, exit 1
	liveAfter []bool   // per start: a scion server runs after it
	logTail   string   // what the log gained, for serverLogTailScript
	nStart    int
	calls     []string
}

func (r *hubStartRunner) live() bool {
	return r.nStart > 0 && r.liveAfter[r.nStart-1]
}

func (r *hubStartRunner) RunIn(_ context.Context, _ string, _ map[string]string, name string, args ...string) (proc.Result, error) {
	fail := func(out string) (proc.Result, error) {
		return proc.Result{Code: 1, Stderr: out}, errors.New("exit status 1")
	}
	if name == "sh" && len(args) >= 2 {
		switch args[1] {
		case serverPIDProbeScript:
			r.calls = append(r.calls, "pid-probe")
			if r.live() {
				return proc.Result{Stdout: "4242"}, nil
			}
			return proc.Result{}, nil
		case serverLogSizeScript:
			r.calls = append(r.calls, "log-size")
			return proc.Result{Stdout: "1000\n"}, nil
		case serverLogTailScript:
			r.calls = append(r.calls, "log-tail "+args[len(args)-1])
			return proc.Result{Stdout: r.logTail}, nil
		}
		return fail("unexpected script")
	}
	if name == "scion" && len(args) >= 2 && args[0] == "server" && args[1] == "start" {
		r.calls = append(r.calls, "start")
		out := r.starts[r.nStart]
		r.nStart++
		if out == "" {
			return proc.Result{Stdout: "Server started"}, nil
		}
		return fail(out)
	}
	if name == "scion" && len(args) > 0 && args[0] == "list" {
		if r.live() {
			return proc.Result{Stdout: "[]"}, nil
		}
		return fail("Error: hub at http://127.0.0.1:8080 is not responding")
	}
	return fail("unscripted " + name)
}

func (r *hubStartRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

func (r *hubStartRunner) RunStdin(ctx context.Context, _ io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

func (r *hubStartRunner) count(call string) int {
	n := 0
	for _, c := range r.calls {
		if c == call {
			n++
		}
	}
	return n
}

func hubStartClient(r *hubStartRunner) *Client {
	c := New(r, Options{})
	c.hubReady.Interval, c.hubAliveEvery, c.hubRetryPause = 0, 0, 0
	return c
}

const (
	scionExitedEarly   = "Starting server as daemon (workstation mode)...\nError: the server exited during startup before it became ready. Check log at: /home/u/.scion/server.log\n"
	scionAlreadyRuns   = "Error: server is already running (PID: 812)\n\nUse 'scion server stop' to stop it, or check the log at /home/u/.scion/server.log\n"
	scionPortsOccupied = "Error: port conflict: ports [8080] are occupied\n"
)

// The failure after a VM reboot: the stale pid file named a pid that a new
// process had taken by the time scion checked it, so scion refused to start
// and started nothing. The refusal is checked against a live scion server,
// and the hub is started once more.
func TestServerStartRestartsAfterAFalseAlreadyRunning(t *testing.T) {
	r := &hubStartRunner{starts: []string{scionAlreadyRuns, ""}, liveAfter: []bool{false, true}}
	var lines []string
	err := hubStartClient(r).ServerStart(context.Background(), ServerOpts{WebPort: 8080, Progress: func(f string, a ...any) {
		lines = append(lines, fmt.Sprintf(f, a...))
	}})
	if err != nil {
		t.Fatalf("ServerStart: %v", err)
	}
	if r.count("start") != 2 {
		t.Fatalf("starts = %d, want 2; calls %v", r.count("start"), r.calls)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "no scion server runs") || !strings.Contains(lines[0], "once more") {
		t.Fatalf("progress = %q", lines)
	}
}

// A true "already running" (a live scion server) is no failure and starts
// nothing more.
func TestServerStartAcceptsATrueAlreadyRunning(t *testing.T) {
	r := &hubStartRunner{starts: []string{scionAlreadyRuns}, liveAfter: []bool{true}}
	if err := hubStartClient(r).ServerStart(context.Background(), ServerOpts{}); err != nil {
		t.Fatalf("ServerStart: %v", err)
	}
	if r.count("start") != 1 {
		t.Fatalf("starts = %d, want 1", r.count("start"))
	}
}

// A hub that exits during start-up twice is started exactly twice, and the
// error carries both attempts with what the log gained since each start.
func TestServerStartRestartsAnExitedHubOnceOnly(t *testing.T) {
	r := &hubStartRunner{
		starts:    []string{scionExitedEarly, scionExitedEarly, ""},
		liveAfter: []bool{false, false, true},
		logTail:   "time=... level=ERROR msg=\"open hub.db: database is locked\"\nexit status 1\n",
	}
	err := hubStartClient(r).ServerStart(context.Background(), ServerOpts{})
	var he *HubExitedError
	if !errors.As(err, &he) || !errors.Is(err, ErrHubNotReady) || he.First == nil {
		t.Fatalf("err = %v, want a HubExitedError with the first attempt", err)
	}
	if r.count("start") != 2 {
		t.Fatalf("starts = %d, want exactly 2; calls %v", r.count("start"), r.calls)
	}
	msg := err.Error()
	for _, want := range []string{"failed to start twice", "First attempt", "database is locked\" | exit status 1", "the server exited during startup"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q: %s", want, msg)
		}
	}
	if r.count("log-tail 1000") != 2 {
		t.Errorf("the log must be read from the size before each start; calls %v", r.calls)
	}
}

// A daemon that scion saw start but that died before the hub answered is the
// same case: lever's own wait finds no server, and starts it once more.
func TestServerStartRestartsAHubGoneDuringTheWait(t *testing.T) {
	r := &hubStartRunner{starts: []string{"", ""}, liveAfter: []bool{false, true}}
	if err := hubStartClient(r).ServerStart(context.Background(), ServerOpts{}); err != nil {
		t.Fatalf("ServerStart: %v", err)
	}
	if r.count("start") != 2 {
		t.Fatalf("starts = %d, want 2", r.count("start"))
	}
}

// Other start failures are not retried: they say nothing about start-up.
func TestServerStartDoesNotRetryOtherFailures(t *testing.T) {
	r := &hubStartRunner{starts: []string{scionPortsOccupied, ""}, liveAfter: []bool{false, true}}
	err := hubStartClient(r).ServerStart(context.Background(), ServerOpts{})
	var he *HubExitedError
	if err == nil || errors.As(err, &he) || r.count("start") != 1 {
		t.Fatalf("err = %v, starts = %d; want the port conflict, once", err, r.count("start"))
	}
}

// The restart waits its pause on the context.
func TestServerStartRestartHonoursCancellation(t *testing.T) {
	r := &hubStartRunner{starts: []string{scionExitedEarly, ""}, liveAfter: []bool{false, true}}
	c := hubStartClient(r)
	c.hubRetryPause = 1 << 40
	ctx, cancel := context.WithCancel(context.Background())
	err := c.ServerStart(ctx, ServerOpts{Progress: func(string, ...any) { cancel() }})
	if !errors.Is(err, context.Canceled) || r.count("start") != 1 {
		t.Fatalf("err = %v, starts = %d", err, r.count("start"))
	}
}

// What the hub's log gained is quoted redacted, secret-free and safe to print,
// on one bounded line.
func TestServerLogSinceIsRedactedAndSafe(t *testing.T) {
	r := &hubStartRunner{logTail: "a\x1b]0;owned\x07b\nsession sess1onsecret1 leaked\ntoken sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789\n" + strings.Repeat("y", 5000) + "\n"}
	c := New(r, Options{})
	got := c.serverLogSince(context.Background(), 0, "sess1onsecret1")
	for _, bad := range []string{"\x1b", "\x07", "sess1onsecret1", "abcdefghijklmnop"} {
		if strings.Contains(got, bad) {
			t.Errorf("log quote keeps %q: %q", bad, got)
		}
	}
	if len(got) > 1600 || strings.Contains(got, "\n") {
		t.Errorf("log quote is %d bytes or multi-line", len(got))
	}
}

// The scripts against a real shell: the log is read only from the given
// offset, a symlinked log is never read, and a pid file naming a dead pid is
// removed (scion would trust that pid once anything reuses it).
func TestHubStartScriptsRunForReal(t *testing.T) {
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("needs /proc")
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, layout.Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(script string, args ...string) string {
		t.Helper()
		cmd := exec.Command("sh", append([]string{"-c", script, "_"}, args...)...)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("script: %v", err)
		}
		return string(out)
	}
	logPath := filepath.Join(home, layout.ServerLogRel)
	if got := strings.TrimSpace(run(serverLogSizeScript, layout.ServerLogRel)); got != "0" {
		t.Fatalf("size of a missing log = %q", got)
	}
	if err := os.WriteFile(logPath, []byte("before reboot\nnew line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(serverLogTailScript, layout.ServerLogRel, "14"); got != "new line\n" {
		t.Fatalf("tail from 14 = %q", got)
	}
	if got := run(serverLogTailScript, layout.ServerLogRel, "999"); got != "before reboot\nnew line\n" {
		t.Fatalf("tail of a shorter log = %q", got)
	}
	secret := filepath.Join(home, "secret")
	if err := os.WriteFile(secret, []byte("hunter2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, logPath); err != nil {
		t.Fatal(err)
	}
	if got := run(serverLogTailScript, layout.ServerLogRel, "0"); got != "" {
		t.Fatalf("a symlinked log was read: %q", got)
	}
	if got := strings.TrimSpace(run(serverLogSizeScript, layout.ServerLogRel)); got != "0" {
		t.Fatalf("a symlinked log was sized: %q", got)
	}

	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(home, layout.ServerPIDRel)
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(dead.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(serverPIDProbeScript, layout.ServerPIDRel); got != "" {
		t.Fatalf("probe of a dead pid printed %q", got)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("a pid file naming a dead pid was kept (stat err %v)", err)
	}
}
