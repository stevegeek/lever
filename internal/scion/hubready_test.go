package scion

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion/layout"
)

func TestWaitHubReadySucceeds(t *testing.T) {
	f := proc.NewFakeRunner()
	// "scion" prefix covers both "server start" and "list --all ...".
	f.Script("scion", proc.Result{Stdout: "ok"})
	c := New(f, Options{})
	c.hubReadyInterval = 0
	if err := c.ServerStart(context.Background(), ServerOpts{WebPort: 8080, DevAuth: false}); err != nil {
		t.Fatalf("ServerStart should succeed when hub is ready: %v", err)
	}
}

func TestWaitHubReadyTimesOut(t *testing.T) {
	f := proc.NewFakeRunner()
	// Leave "list --all" unscripted so the probe errors every attempt.
	c := New(f, Options{})
	c.hubReadyTimeout, c.hubReadyInterval = 20*time.Millisecond, time.Millisecond
	err := c.waitHubReady(context.Background(), nil)
	if !errors.Is(err, ErrHubNotReady) {
		t.Fatalf("expected ErrHubNotReady when hub never comes up, got %v", err)
	}
}

// TestWaitRuntimeBrokerReadyReturnsWhenOnline: an online broker in the listing
// resolves the gate immediately (one hub call, no error).
func TestWaitRuntimeBrokerReadyReturnsWhenOnline(t *testing.T) {
	assertBrokerGateStopsAtOnline(t, `[{"status":"online","connectionState":"connected"}]`)
}

// cliCalls counts the scion CLI calls made from the host.
func cliCalls(f *proc.FakeRunner) int {
	n := 0
	for _, c := range f.Calls {
		if c.Name == "scion" {
			n++
		}
	}
	return n
}

func assertBrokerGateStopsAtOnline(t *testing.T, out string) {
	t.Helper()
	f := proc.NewFakeRunner()
	f.Script("scion hub brokers", proc.Result{Stdout: out})
	c := New(f, Options{})
	c.brokerReadyInterval = 0
	if err := c.WaitRuntimeBrokerReady(context.Background(), "/lever"); err != nil {
		t.Fatalf("WaitRuntimeBrokerReady should return nil when a broker is online: %v", err)
	}
	if n := cliCalls(f); n != 1 {
		t.Errorf("hub-brokers calls = %d, want 1 (must stop as soon as a broker is online)", n)
	}
}

// TestWaitRuntimeBrokerReadyStripsDevAuthBanner: the dev-auth banner in the
// same stream must not make the parse fail and the gate silently no-op.
func TestWaitRuntimeBrokerReadyStripsDevAuthBanner(t *testing.T) {
	assertBrokerGateStopsAtOnline(t,
		"WARNING: development auth is enabled — do not use in production\n[{\"status\":\"online\",\"connectionState\":\"connected\"}]")
}

// TestWaitRuntimeBrokerReadyOfflineIsNotReadyThenFailSoft: a registered but
// offline broker does not satisfy the gate, and exhaustion returns nil.
func TestWaitRuntimeBrokerReadyOfflineIsNotReadyThenFailSoft(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion hub brokers", proc.Result{Stdout: `[{"status":"offline","connectionState":"disconnected"}]`})
	c := New(f, Options{})
	c.brokerReadyAttempts, c.brokerReadyInterval = 3, 0
	if err := c.WaitRuntimeBrokerReady(context.Background(), "/lever"); err != nil {
		t.Fatalf("gate must be fail-soft (nil) on exhaustion, got: %v", err)
	}
	if n := cliCalls(f); n != 3 {
		t.Errorf("hub-brokers calls = %d, want 3 (an offline broker must not satisfy the gate)", n)
	}
}

// TestWaitRuntimeBrokerReadyCtxCancel: a cancelled context returns its error
// promptly rather than burning the budget.
func TestWaitRuntimeBrokerReadyCtxCancel(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion hub brokers", proc.Result{Stdout: `[]`}) // never ready
	c := New(f, Options{})
	c.brokerReadyAttempts, c.brokerReadyInterval = 30, time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.WaitRuntimeBrokerReady(ctx, "/lever"); err == nil {
		t.Fatal("a cancelled context must return an error, not fail-soft nil")
	}
}

// refusingRunner answers every command like a scion CLI whose hub is up but
// refuses the credential (HTTP 401).
type refusingRunner struct {
	stderr string
	calls  int
}

func (r *refusingRunner) RunIn(context.Context, string, map[string]string, string, ...string) (proc.Result, error) {
	r.calls++
	return proc.Result{Code: 1, Stderr: r.stderr}, errors.New("exit status 1")
}

func (r *refusingRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

func (r *refusingRunner) RunStdin(ctx context.Context, _ io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// A hub that answers 401 is up: lever may hold no working token for it yet
// (a first mint that failed). It must not be reported as not ready.
func TestWaitHubReadyAcceptsAuthRefusal(t *testing.T) {
	r := &refusingRunner{stderr: "Error: authentication failed, login to hub with 'scion hub auth login'\n"}
	c := New(r, Options{})
	c.hubReadyInterval = 0
	if err := c.waitHubReady(context.Background(), nil); err != nil {
		t.Fatalf("a hub refusing the credential is up; got %v", err)
	}
	if r.calls != 1 {
		t.Fatalf("probe calls = %d, want 1", r.calls)
	}
}

// Anything else still counts as not ready.
func TestWaitHubReadyOtherErrorsNotReady(t *testing.T) {
	r := &refusingRunner{stderr: "Error: hub at http://127.0.0.1:8080 is not responding\n"}
	c := New(r, Options{})
	c.hubReadyTimeout, c.hubReadyInterval = 20*time.Millisecond, time.Millisecond
	if err := c.waitHubReady(context.Background(), nil); !errors.Is(err, ErrHubNotReady) {
		t.Fatalf("want ErrHubNotReady, got %v", err)
	}
}

// A cold hub that answers only after many failed probes is waited on: the
// budget is a time, not an attempt count.
func TestWaitHubReadyOutlastsAColdStart(t *testing.T) {
	r := &coldHubRunner{failures: 40}
	c := New(r, Options{})
	c.hubReadyInterval = 0
	var lines []string
	start := time.Now()
	err := c.waitHubReady(context.Background(), func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatalf("a hub that answers on probe 41 is ready within the budget; got %v", err)
	}
	if r.probes != 41 {
		t.Fatalf("probes = %d, want 41", r.probes)
	}
	if time.Since(start) < c.hubReadyNotice && len(lines) != 0 {
		t.Fatalf("progress before the notice delay: %q", lines)
	}
}

// Progress is printed once the wait passes the notice delay, naming the
// budget.
func TestWaitHubReadyPrintsProgress(t *testing.T) {
	r := &coldHubRunner{failures: 1 << 30, slow: 5 * time.Millisecond}
	c := New(r, Options{})
	c.hubReadyTimeout, c.hubReadyInterval, c.hubReadyNotice = 60*time.Millisecond, 0, 10*time.Millisecond
	var lines []string
	err := c.waitHubReady(context.Background(), func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	if !errors.Is(err, ErrHubNotReady) || !strings.Contains(err.Error(), "attempts") {
		t.Fatalf("err = %v, want ErrHubNotReady with the attempt count", err)
	}
	if len(lines) == 0 || !strings.Contains(lines[0], "not answering yet") || !strings.Contains(lines[0], "waiting up to") {
		t.Fatalf("progress = %q", lines)
	}
}

// When the server process is gone, the wait stops early instead of using
// the whole budget: nothing will ever answer.
func TestWaitHubReadyStopsWhenTheServerIsGone(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("sh -c f=", proc.Result{}) // the probe: no live scion server
	c := New(f, Options{})
	// A budget long enough that only the liveness check can end the wait
	// early, short enough that a regression fails here, not at go test's
	// timeout: running it out reads as "after 2s", without "server.log".
	c.hubReadyTimeout, c.hubReadyInterval, c.hubAliveEvery = 2*time.Second, time.Millisecond, 0
	start := time.Now()
	err := c.waitHubReady(context.Background(), nil)
	if !errors.Is(err, ErrHubNotReady) || !strings.Contains(err.Error(), "server.log") {
		t.Fatalf("err = %v, want ErrHubNotReady pointing at the server log", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("the wait did not stop when the server was gone")
	}
}

// A live server keeps the wait going; a probe that cannot run is no verdict.
func TestWaitHubReadyKeepsWaitingOnALiveServer(t *testing.T) {
	for _, live := range []bool{true, false} {
		f := proc.NewFakeRunner()
		if live {
			f.Script("sh -c f=", proc.Result{Stdout: "4242"})
		} // else unscripted: the fake fails the probe, as a failed jail shell would
		c := New(f, Options{})
		c.hubReadyTimeout, c.hubReadyInterval, c.hubAliveEvery = 30*time.Millisecond, time.Millisecond, 0
		err := c.waitHubReady(context.Background(), nil)
		if !errors.Is(err, ErrHubNotReady) || strings.Contains(err.Error(), "server.log") {
			t.Fatalf("live=%v: err = %v, want the budget to run out", live, err)
		}
	}
}

// ServerStart clears a stale pid file BEFORE the start: a file naming a live
// non-scion process would make scion answer "already running" and start
// nothing.
func TestServerStartProbesThePidFileFirst(t *testing.T) {
	f, c := okScion()
	f.Script("sh -c f=", proc.Result{Stdout: "stale:77"})
	if err := c.ServerStart(context.Background(), ServerOpts{WebPort: 8080}); err != nil {
		t.Fatal(err)
	}
	iProbe := f.CallIndex(proc.ArgvContains(layout.ServerPIDRel))
	iStart := f.CallIndex(proc.ArgvPrefix("scion", "server", "start"))
	if iProbe < 0 || iStart < 0 || iProbe > iStart {
		t.Fatalf("want the pid probe before the start; calls=%+v", f.Calls)
	}
}

type coldHubRunner struct {
	failures, probes int
	slow             time.Duration
}

func (r *coldHubRunner) RunIn(_ context.Context, _ string, _ map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "sh" {
		return proc.Result{Code: 1}, errors.New("exit status 1")
	}
	r.probes++
	time.Sleep(r.slow)
	if r.probes <= r.failures {
		return proc.Result{Code: 1, Stderr: "Error: hub at http://127.0.0.1:8080 is not responding\n"}, errors.New("exit status 1")
	}
	return proc.Result{Stdout: "[]"}, nil
}

func (r *coldHubRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

func (r *coldHubRunner) RunStdin(ctx context.Context, _ io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// inGuestRunner answers the in-guest waits (`sh -c <script> …`) from a queue
// of results, one per call, and everything else through the FakeRunner.
type inGuestRunner struct {
	*proc.FakeRunner
	waits []proc.Result
	seen  [][]string
	// lists, when set, answers `scion list` in turn (then falls through).
	lists []proc.Result
}

func (r *inGuestRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "sh" && len(args) > 1 && (strings.Contains(args[1], "healthz") || strings.Contains(args[1], "case $out")) {
		r.seen = append(r.seen, args)
		r.Calls = append(r.Calls, proc.Call{Name: name, Args: args, Env: env})
		if len(r.waits) == 0 {
			return proc.Result{Code: waitedExit}, errors.New("exit status 3")
		}
		res := r.waits[0]
		r.waits = r.waits[1:]
		if res.Code != 0 {
			return res, fmt.Errorf("exit status %d", res.Code)
		}
		return res, nil
	}
	if name == "scion" && len(args) > 0 && args[0] == "list" && len(r.lists) > 0 {
		r.Calls = append(r.Calls, proc.Call{Name: name, Args: args, Env: env})
		res := r.lists[0]
		r.lists = r.lists[1:]
		if res.Code != 0 {
			return res, fmt.Errorf("exit status %d", res.Code)
		}
		return res, nil
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *inGuestRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// The broker wait is one in-guest loop; an online broker it prints ends the
// wait with no host-side scion call.
func TestRuntimeBrokerReadyWaitsInTheGuest(t *testing.T) {
	r := &inGuestRunner{FakeRunner: proc.NewFakeRunner(), waits: []proc.Result{{Stdout: `[{"status":"online"}]`}}}
	c := New(r, Options{HubTokenSource: func() string { return "tok" }})
	online, err := c.RuntimeBrokerReady(context.Background(), "global")
	if err != nil || !online {
		t.Fatalf("online=%v err=%v", online, err)
	}
	if len(r.seen) != 1 || cliCalls(r.FakeRunner) != 0 {
		t.Fatalf("in-guest calls %d, host scion calls %d; want 1 and 0", len(r.seen), cliCalls(r.FakeRunner))
	}
	args := strings.Join(r.seen[0], " ")
	// 30 s at 200 ms, then the scion command the loop runs.
	_, tail, _ := strings.Cut(args, " _ ")
	n, rest, _ := strings.Cut(tail, " ")
	if k, err := strconv.Atoi(n); err != nil || k < 140 || k > 150 || rest != "scion hub brokers --format json -g global" {
		t.Fatalf("in-guest argv: %s", args)
	}
	// The hub token rides the env (the jail runner keeps it off the argv).
	if r.Calls[0].Env["SCION_HUB_TOKEN"] != "tok" {
		t.Fatal("the in-guest wait must carry the scion env")
	}
}

// The guest's text match only ends its loop; lever's own parse decides, and
// keeps waiting when it does not hold.
func TestRuntimeBrokerReadyParsesWhatTheGuestPrinted(t *testing.T) {
	r := &inGuestRunner{FakeRunner: proc.NewFakeRunner(), waits: []proc.Result{
		{Stdout: `[{"name":"online","status":"offline"}]`},
		{Stdout: `[{"status":"online"}]`},
	}}
	c := New(r, Options{})
	online, err := c.RuntimeBrokerReady(context.Background(), "global")
	if err != nil || !online || len(r.seen) != 2 {
		t.Fatalf("online=%v err=%v calls=%d; want the second answer to decide", online, err, len(r.seen))
	}
}

// A wait that ran its whole window is fail-soft (false, nil), and does not
// start the host polling over again.
func TestRuntimeBrokerReadyInGuestTimeoutIsFailSoft(t *testing.T) {
	r := &inGuestRunner{FakeRunner: proc.NewFakeRunner()}
	c := New(r, Options{})
	c.brokerReadyAttempts, c.brokerReadyInterval = 1, 0
	online, err := c.RuntimeBrokerReady(context.Background(), "global")
	if err != nil || online {
		t.Fatalf("online=%v err=%v", online, err)
	}
	if cliCalls(r.FakeRunner) != 0 {
		t.Fatal("a guest wait that ran must not be repeated from the host")
	}
}

// When the in-guest wait cannot run (a transport error), it polls from the
// host.
func TestRuntimeBrokerReadyFallsBackToHostPolling(t *testing.T) {
	r := &inGuestRunner{FakeRunner: proc.NewFakeRunner(), waits: []proc.Result{{Code: 255}}}
	r.Script("scion hub brokers", proc.Result{Stdout: `[{"status":"online"}]`})
	c := New(r, Options{})
	online, err := c.RuntimeBrokerReady(context.Background(), "global")
	if err != nil || !online || cliCalls(r.FakeRunner) != 1 {
		t.Fatalf("online=%v err=%v host calls=%d", online, err, cliCalls(r.FakeRunner))
	}
}

// With an endpoint the hub wait polls /healthz in the guest, and one CLI probe
// after it decides.
func TestWaitHubReadyPollsHealthzInTheGuest(t *testing.T) {
	r := &inGuestRunner{FakeRunner: proc.NewFakeRunner(), waits: []proc.Result{{Code: waitedExit}, {}},
		lists: []proc.Result{{Code: 1, Stderr: "Error: hub at http://127.0.0.1:8080 is not responding"}}}
	r.Script("scion list --all", proc.Result{Stdout: "[]"})
	c := New(r, Options{HubEndpoint: "http://127.0.0.1:8080/"})
	c.hubReadyInterval = time.Hour // must not be slept between guest waits
	if err := c.waitHubReady(context.Background(), nil); err != nil {
		t.Fatalf("waitHubReady: %v", err)
	}
	if len(r.seen) != 2 {
		t.Fatalf("in-guest waits = %d, want 2", len(r.seen))
	}
	if got := strings.Join(r.seen[0][2:4], " "); got != "_ http://127.0.0.1:8080" {
		t.Fatalf("healthz args = %q", got)
	}
	// One CLI probe after each guest wait: the first finds the hub down,
	// the second decides.
	if n := cliCalls(r.FakeRunner); n != 2 {
		t.Fatalf("CLI probes = %d, want 2", n)
	}
}

// Without curl in the jail the wait polls with the CLI.
func TestWaitHubReadyWithoutCurlPollsTheCLI(t *testing.T) {
	r := &inGuestRunner{FakeRunner: proc.NewFakeRunner(), waits: []proc.Result{{Code: 127}}}
	c := New(r, Options{HubEndpoint: "http://127.0.0.1:8080"})
	c.hubReadyTimeout, c.hubReadyInterval = 30*time.Millisecond, time.Millisecond
	if err := c.waitHubReady(context.Background(), nil); !errors.Is(err, ErrHubNotReady) {
		t.Fatalf("want ErrHubNotReady, got %v", err)
	}
	if len(r.seen) != 1 || cliCalls(r.FakeRunner) < 2 {
		t.Fatalf("guest waits %d (want 1), CLI probes %d (want several)", len(r.seen), cliCalls(r.FakeRunner))
	}
}

// The in-guest scripts, run for real: the broker loop stops at an online
// broker and prints it, and exits waitedExit when none appears; the healthz
// loop exits waitedExit against a port nothing listens on.
func TestInGuestWaitScriptsRunForReal(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	out, err := exec.Command(sh, "-c", brokerWaitScript, "_", "3", "echo", `[{"status":"online"}]`).Output()
	if err != nil || string(out) != `[{"status":"online"}]` {
		t.Fatalf("online: %q %v", out, err)
	}
	err = exec.Command(sh, "-c", brokerWaitScript, "_", "1", "echo", `[{"status":"offline"}]`).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != waitedExit {
		t.Fatalf("offline: %v, want exit %d", err, waitedExit)
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	err = exec.Command(sh, "-c", healthzScript, "_", "http://127.0.0.1:9", "1").Run()
	if !errors.As(err, &ee) || ee.ExitCode() != waitedExit {
		t.Fatalf("healthz: %v, want exit %d", err, waitedExit)
	}
	// Something that answers, but not 200: unusable at once (exit 4).
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	err = exec.Command(sh, "-c", healthzScript, "_", srv.URL, "20").Run()
	if !errors.As(err, &ee) || ee.ExitCode() != 4 {
		t.Fatalf("healthz 404: %v, want exit 4", err)
	}
}
