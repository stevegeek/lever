package scion

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/proc"
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
	c.hubReadyAttempts, c.hubReadyInterval = 2, 0
	err := c.waitHubReady(context.Background())
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
	c.hubReadyAttempts, c.hubReadyInterval = 5, 0
	if err := c.waitHubReady(context.Background()); err != nil {
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
	c.hubReadyAttempts, c.hubReadyInterval = 2, 0
	if err := c.waitHubReady(context.Background()); !errors.Is(err, ErrHubNotReady) {
		t.Fatalf("want ErrHubNotReady, got %v", err)
	}
}
