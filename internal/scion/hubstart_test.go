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
	"syscall"
	"testing"
	"time"

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
	got, err := c.serverLogSince(context.Background(), 0, "sess1onsecret1")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"\x1b", "\x07", "sess1onsecret1", "abcdefghijklmnop"} {
		if strings.Contains(got, bad) {
			t.Errorf("log quote keeps %q: %q", bad, got)
		}
	}
	if len(got) > 1600 || strings.Contains(got, "\n") {
		t.Errorf("log quote is %d bytes or multi-line", len(got))
	}
}

// A tail that filled its window drops its first line, which may hold only
// the end of a secret.
func TestServerLogSinceDropsACutFirstLine(t *testing.T) {
	r := &hubStartRunner{logTail: "cret1 tail of a cut line\n" + strings.Repeat("z\n", serverLogTailBytes/2) + "last\n"}
	got, err := New(r, Options{}).serverLogSince(context.Background(), 0, "sess1onsecret1")
	if err != nil || strings.Contains(got, "cret1") || !strings.Contains(got, "last") {
		t.Fatalf("got %q, %v", got, err)
	}
}

// A log lever could not read is said so, never reported as empty.
func TestServerStartSaysWhenTheLogWasUnread(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{{logSymlinkExit, "symlink"}, {255, "the read in the guest failed"}} {
		r := &unreadLogRunner{hubStartRunner: hubStartRunner{starts: []string{scionExitedEarly, scionExitedEarly}, liveAfter: []bool{false, false}}, code: tc.code}
		c := New(r, Options{})
		c.hubReady.Interval, c.hubAliveEvery, c.hubRetryPause = 0, 0, 0
		err := c.ServerStart(context.Background(), ServerOpts{})
		if err == nil || !strings.Contains(err.Error(), "could not read") || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "wrote nothing") {
			t.Fatalf("code %d: %v", tc.code, err)
		}
	}
}

// unreadLogRunner fails both log scripts with code.
type unreadLogRunner struct {
	hubStartRunner
	code int
}

func (r *unreadLogRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "sh" && len(args) >= 2 && (args[1] == serverLogSizeScript || args[1] == serverLogTailScript) {
		return proc.Result{Code: r.code}, fmt.Errorf("exit status %d", r.code)
	}
	return r.hubStartRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *unreadLogRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
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
	for _, script := range []string{serverLogTailScript, serverLogSizeScript} {
		cmd := exec.Command("sh", "-c", script, "_", layout.ServerLogRel, "0")
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.Output()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != logSymlinkExit || len(out) != 0 {
			t.Fatalf("a symlinked log was read: %q %v", out, err)
		}
	}

	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(home, layout.ServerPIDRel)
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(dead.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(serverPIDProbeScript, layout.ServerPIDRel, layout.ScionBin); got != "" {
		t.Fatalf("probe of a dead pid printed %q", got)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("a pid file naming a dead pid was kept (stat err %v)", err)
	}

	// A pid file scion's Atoi refuses stops every later server start: an
	// empty one (a guest killed between its create and its write), one with
	// a trailing newline, and one with other bytes are removed.
	for _, body := range []string{"", strconv.Itoa(os.Getpid()) + "\n", "12ab", " 1", strings.Repeat("1", 40)} {
		if err := os.WriteFile(pidFile, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := run(serverPIDProbeScript, layout.ServerPIDRel, layout.ScionBin); got != "" {
			t.Fatalf("probe of the pid file %q printed %q", body, got)
		}
		if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
			t.Fatalf("the pid file %q was kept (stat err %v)", body, err)
		}
	}
}

// A process whose argv reads as a scion server counts only when it runs the
// installed binary: a decoy is not a server, and its pid file is removed.
func TestServerPIDProbeRefusesADecoy(t *testing.T) {
	if _, err := os.Stat("/proc/self/ns/pid"); err != nil {
		t.Skip("needs /proc")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("needs bash")
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, layout.Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	decoy := exec.Command(bash, "-c", `exec -a "scion server start" sleep 30`)
	if err := decoy.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = decoy.Process.Kill(); _ = decoy.Wait() }()
	pid := strconv.Itoa(decoy.Process.Pid)
	pidFile := filepath.Join(home, layout.ServerPIDRel)
	probe := func(bin string) string {
		t.Helper()
		if err := os.WriteFile(pidFile, []byte(pid), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", serverPIDProbeScript, "_", layout.ServerPIDRel, bin)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		return string(out)
	}
	// Wait for the exec, so the exe link names sleep.
	sleepBin := ""
	for range 200 {
		if exe, err := os.Readlink("/proc/" + pid + "/exe"); err == nil && strings.Contains(filepath.Base(exe), "sleep") {
			sleepBin = exe
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sleepBin == "" {
		t.Skip("could not read the decoy's exe")
	}
	if got := probe(sleepBin); got != pid {
		t.Fatalf("the installed binary with a server argv: probe printed %q, want %s", got, pid)
	}
	if got := probe(layout.ScionBin); got != "stale:"+pid {
		t.Fatalf("a decoy: probe printed %q, want stale:%s", got, pid)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("a decoy's pid file was kept (stat err %v)", err)
	}
}

// A pid file that names no live server, beside a running hub (an agent can
// write the file under a live server), is rewritten to the one scion server
// that runs, not removed: removing it would orphan the hub. Malformed bytes
// of any length, a dead pid, a decoy's pid and a non-regular file are all
// repaired. Two servers make the repair ambiguous, and the file is removed.
func TestServerPIDProbeRepairsAFileBesideALiveServer(t *testing.T) {
	if _, err := os.Stat("/proc/self/ns/pid"); err != nil {
		t.Skip("needs /proc")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("needs bash")
	}
	home := t.TempDir()
	dir := filepath.Join(home, layout.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	startServer := func() (string, string) {
		t.Helper()
		c := exec.Command(bash, "-c", `exec -a "scion server start" sleep 30`)
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
		pid := strconv.Itoa(c.Process.Pid)
		for range 200 {
			if exe, err := os.Readlink("/proc/" + pid + "/exe"); err == nil && strings.Contains(filepath.Base(exe), "sleep") {
				return pid, exe
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Skip("could not read the server's exe")
		return "", ""
	}
	pid, bin := startServer()
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(home, layout.ServerPIDRel)
	run := func() string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", serverPIDProbeScript, "_", layout.ServerPIDRel, bin)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		return string(out)
	}
	repaired := func(what string) {
		t.Helper()
		if got := run(); got != pid {
			t.Fatalf("%s: probe printed %q, want %s", what, got, pid)
		}
		fi, err := os.Lstat(pidFile)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o644 {
			t.Fatalf("%s: pid file after the probe: %v %v, want a 0644 regular file", what, fi, err)
		}
		if b, _ := os.ReadFile(pidFile); string(b) != pid {
			t.Fatalf("%s: pid file holds %q, want exactly %s", what, b, pid)
		}
	}
	for _, body := range []string{
		pid + "\n", pid + "\r\n", " " + pid, "\t" + pid + "\n\n",
		pid + strings.Repeat(" ", 40), "x", "", pid + "x", strings.Repeat("9", 40),
		strconv.Itoa(dead.Process.Pid), strconv.Itoa(os.Getpid()),
	} {
		if err := os.WriteFile(pidFile, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		repaired(fmt.Sprintf("body %q", body))
	}
	for name, mk := range map[string]func() error{
		"fifo":      func() error { return syscall.Mkfifo(pidFile, 0o644) },
		"directory": func() error { return os.Mkdir(pidFile, 0o755) },
		"symlink":   func() error { return os.Symlink(filepath.Join(home, "none"), pidFile) },
	} {
		if err := os.Remove(pidFile); err != nil {
			t.Fatal(err)
		}
		if err := mk(); err != nil {
			t.Fatal(err)
		}
		repaired(name)
	}
	// A missing file is left alone.
	if err := os.Remove(pidFile); err != nil {
		t.Fatal(err)
	}
	if got := run(); got != "" {
		t.Fatalf("a missing file: probe printed %q", got)
	}
	if _, err := os.Lstat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("a missing file was created (lstat err %v)", err)
	}
	// Two servers: no repair, the bad file goes.
	startServer()
	if err := os.WriteFile(pidFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(); got != "" {
		t.Fatalf("two servers: probe printed %q, want nothing", got)
	}
	if _, err := os.Lstat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("two servers: the bad file was kept (lstat err %v)", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		t.Errorf("left behind in %s: %s", dir, e.Name())
	}
}

// Anything at the pid path that is not a regular file is removed without
// being read: scion's os.ReadFile blocks forever on a FIFO. A symlink is
// removed even when it names a valid pid file, and its target is kept.
func TestServerPIDProbeRemovesNonRegularFiles(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, layout.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(home, layout.ServerPIDRel)
	target := filepath.Join(home, "target")
	if err := os.WriteFile(target, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(home, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	cases := []struct {
		name string
		make func() error
	}{
		{"fifo", func() error { return syscall.Mkfifo(pidFile, 0o644) }},
		{"directory", func() error {
			if err := os.Mkdir(pidFile, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(pidFile, "x"), []byte("1"), 0o644)
		}},
		{"symlink to a pid file", func() error { return os.Symlink(target, pidFile) }},
		{"symlink to a fifo", func() error { return os.Symlink(fifo, pidFile) }},
		{"dangling symlink", func() error { return os.Symlink(filepath.Join(home, "none"), pidFile) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.make(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", serverPIDProbeScript, "_", layout.ServerPIDRel, layout.ScionBin)
			cmd.Env = append(os.Environ(), "HOME="+home)
			out, err := cmd.Output()
			if ctx.Err() != nil {
				t.Fatal("the probe hung")
			}
			if err != nil || len(out) != 0 {
				t.Fatalf("probe printed %q, err %v", out, err)
			}
			if _, err := os.Lstat(pidFile); !os.IsNotExist(err) {
				t.Fatalf("the %s was kept (lstat err %v)", tc.name, err)
			}
		})
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("a symlink's target was touched: %v", err)
	}
}
