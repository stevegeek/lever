package scion

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
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
	c.hubReady.Interval = 0
	if err := c.ServerStart(context.Background(), ServerOpts{WebPort: 8080, DevAuth: false}); err != nil {
		t.Fatalf("ServerStart should succeed when hub is ready: %v", err)
	}
}

func TestWaitHubReadyTimesOut(t *testing.T) {
	f := proc.NewFakeRunner()
	// Leave "list --all" unscripted so the probe errors every attempt.
	c := New(f, Options{})
	c.hubReady.Budget, c.hubReady.Interval = 20*time.Millisecond, time.Millisecond
	err := c.waitHubReady(context.Background(), nil)
	if !errors.Is(err, ErrHubNotReady) {
		t.Fatalf("expected ErrHubNotReady when hub never comes up, got %v", err)
	}
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
	c.hubReady.Interval = 0
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
	c.hubReady.Budget, c.hubReady.Interval = 20*time.Millisecond, time.Millisecond
	if err := c.waitHubReady(context.Background(), nil); !errors.Is(err, ErrHubNotReady) {
		t.Fatalf("want ErrHubNotReady, got %v", err)
	}
}

// A cold hub that answers only after many failed probes is waited on: the
// budget is a time, not an attempt count.
func TestWaitHubReadyOutlastsAColdStart(t *testing.T) {
	r := &coldHubRunner{failures: 40}
	c := New(r, Options{})
	c.hubReady.Interval = 0
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
	if time.Since(start) < c.hubReady.NoticeAfter && len(lines) != 0 {
		t.Fatalf("progress before the notice delay: %q", lines)
	}
}

// Progress is printed once the wait passes the notice delay, naming the
// budget.
func TestWaitHubReadyPrintsProgress(t *testing.T) {
	r := &coldHubRunner{failures: 1 << 30, slow: 5 * time.Millisecond}
	c := New(r, Options{})
	c.hubReady.Budget, c.hubReady.Interval, c.hubReady.NoticeAfter = 60*time.Millisecond, 0, 10*time.Millisecond
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
	c.hubReady.Budget, c.hubReady.Interval, c.hubAliveEvery = 2*time.Second, time.Millisecond, 0
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
		c.hubReady.Budget, c.hubReady.Interval, c.hubAliveEvery = 30*time.Millisecond, time.Millisecond, 0
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

// With an endpoint the hub wait polls /healthz in the guest, and one CLI probe
// after it decides.
func TestWaitHubReadyPollsHealthzInTheGuest(t *testing.T) {
	r := &inGuestRunner{FakeRunner: proc.NewFakeRunner(), waits: []proc.Result{{Code: waitedExit}, {}},
		lists: []proc.Result{{Code: 1, Stderr: "Error: hub at http://127.0.0.1:8080 is not responding"}}}
	r.Script("scion list --all", proc.Result{Stdout: "[]"})
	c := New(r, Options{HubEndpoint: "http://127.0.0.1:8080/"})
	c.hubReady.Interval = time.Hour // must not be slept between guest waits
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
	c.hubReady.Budget, c.hubReady.Interval = 30*time.Millisecond, time.Millisecond
	if err := c.waitHubReady(context.Background(), nil); !errors.Is(err, ErrHubNotReady) {
		t.Fatalf("want ErrHubNotReady, got %v", err)
	}
	if len(r.seen) != 1 || cliCalls(r.FakeRunner) < 2 {
		t.Fatalf("guest waits %d (want 1), CLI probes %d (want several)", len(r.seen), cliCalls(r.FakeRunner))
	}
}

// The in-guest healthz script, run for real: it exits waitedExit against a
// port nothing listens on.
func TestInGuestWaitScriptRunsForReal(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	var ee *exec.ExitError
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

// cappedRunner answers every command the way the jail runner does once a
// command's output passes its cap: the output swapped for a line naming the
// cap, and an error that wraps proc.ErrOutputLimit. Only list is capped.
type cappedRunner struct{ calls int }

func (r *cappedRunner) RunIn(_ context.Context, _ string, _ map[string]string, _ string, args ...string) (proc.Result, error) {
	if len(args) == 0 || args[0] != "list" {
		return proc.Result{}, nil
	}
	r.calls++
	return proc.Result{Code: -1, Stderr: "the command's output passed lever's 8388608-byte limit for a jail command; it was killed"},
		fmt.Errorf("limactl: %w", proc.ErrOutputLimit)
}

func (r *cappedRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

func (r *cappedRunner) RunStdin(ctx context.Context, _ io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// An agent list over the cap is the hub answering with a list a retry reads
// again: the wait must end on the first probe, not run out its budget.
func TestWaitHubReadyFailsAtOnceOnOversizedList(t *testing.T) {
	r := &cappedRunner{}
	c := New(r, Options{})
	c.hubReady.Budget, c.hubReady.Interval = 2*time.Second, time.Millisecond
	start := time.Now()
	err := c.waitHubReady(context.Background(), nil)
	if !errors.Is(err, ErrAgentListTooLarge) || !errors.Is(err, proc.ErrOutputLimit) {
		t.Fatalf("err = %v, want ErrAgentListTooLarge wrapping proc.ErrOutputLimit", err)
	}
	if errors.Is(err, ErrHubNotReady) {
		t.Fatalf("the hub answered; err must not read as not ready: %v", err)
	}
	if r.calls != 1 || time.Since(start) > time.Second {
		t.Fatalf("calls = %d after %s, want one probe and an immediate failure", r.calls, time.Since(start))
	}
	for _, want := range []string{"agent list is too large", "lever worker purge", "lever up --fresh", "GET /api/v1/agents", "8388608-byte limit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func TestServerStartFailsAtOnceOnOversizedList(t *testing.T) {
	r := &cappedRunner{}
	c := New(r, Options{})
	c.hubReady.Budget, c.hubReady.Interval = 2*time.Second, time.Millisecond
	c.hubRetryPause = time.Second
	start := time.Now()
	err := c.ServerStart(context.Background(), ServerOpts{})
	if !errors.Is(err, ErrAgentListTooLarge) || r.calls != 1 || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %d calls and %s, want ErrAgentListTooLarge at once", err, r.calls, time.Since(start))
	}
}
