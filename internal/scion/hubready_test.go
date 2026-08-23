package scion

import (
	"context"
	"errors"
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
