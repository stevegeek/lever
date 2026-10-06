package scion

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// assertBrokerGateStopsAtOnline scripts `scion hub brokers` with out and
// checks the gate returns nil after exactly one call.
func assertBrokerGateStopsAtOnline(t *testing.T, out string) {
	t.Helper()
	f := proc.NewFakeRunner()
	f.Script("scion hub brokers", proc.Result{Stdout: out})
	c := New(f, Options{})
	c.brokerReadyInterval = 0
	if err := c.WaitRuntimeBrokerReady(context.Background(), "/lever"); err != nil {
		t.Fatalf("WaitRuntimeBrokerReady should return nil when a broker is online: %v", err)
	}
	if len(f.Calls) != 1 {
		t.Errorf("hub-brokers calls = %d, want 1 (must stop as soon as a broker is online)", len(f.Calls))
	}
}

// TestWaitRuntimeBrokerReadyStripsDevAuthBanner: under dev-auth-ON scion prints
// the WARNING banner into the same stream as the JSON; the gate must strip it
// (via parseJSON, like List/messaging) and still see the online broker, rather
// than failing the parse and silently no-opping.
func TestWaitRuntimeBrokerReadyStripsDevAuthBanner(t *testing.T) {
	assertBrokerGateStopsAtOnline(t,
		"WARNING: development auth is enabled — do not use in production\n[{\"status\":\"online\",\"connectionState\":\"connected\"}]")
}

// TestWaitRuntimeBrokerReadyOfflineIsNotReadyThenFailSoft: a broker that is
// registered but NOT online must not satisfy the gate — it keeps polling the
// whole budget — and on exhaustion the gate is fail-soft (returns nil, never an
// error, so it can't fail the bring-up; the start-path retry backstops).
func TestWaitRuntimeBrokerReadyOfflineIsNotReadyThenFailSoft(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion hub brokers", proc.Result{Stdout: `[{"status":"offline","connectionState":"disconnected"}]`})
	c := New(f, Options{})
	c.brokerReadyAttempts, c.brokerReadyInterval = 3, 0
	if err := c.WaitRuntimeBrokerReady(context.Background(), "/lever"); err != nil {
		t.Fatalf("gate must be fail-soft (nil) on exhaustion, got: %v", err)
	}
	if len(f.Calls) != 3 {
		t.Errorf("hub-brokers calls = %d, want 3 (an offline broker must not satisfy the gate)", len(f.Calls))
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
// budget is a time, not the old 30 attempts, and a slow start prints that
// it is waiting.
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

// coldHubRunner fails the hub probe failures times (connection refused),
// then answers.
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
