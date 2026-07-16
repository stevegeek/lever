package scion

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/exec"
)

func TestWaitHubReadySucceeds(t *testing.T) {
	saveAttempts, saveInterval := hubReadyAttempts, hubReadyInterval
	defer func() { hubReadyAttempts, hubReadyInterval = saveAttempts, saveInterval }()
	hubReadyInterval = 0

	f := exec.NewFakeRunner()
	// "scion" prefix covers both "server start" and "list --all ...".
	f.Script("scion", exec.Result{Stdout: "ok"})
	c := New(f, Options{})
	if err := c.ServerStart(context.Background(), ServerOpts{WebPort: 8080, DevAuth: false}); err != nil {
		t.Fatalf("ServerStart should succeed when hub is ready: %v", err)
	}
}

func TestWaitHubReadyTimesOut(t *testing.T) {
	saveAttempts, saveInterval := hubReadyAttempts, hubReadyInterval
	defer func() { hubReadyAttempts, hubReadyInterval = saveAttempts, saveInterval }()
	hubReadyAttempts, hubReadyInterval = 2, 0

	f := exec.NewFakeRunner()
	// Leave "list --all" unscripted so the probe errors every attempt.
	c := New(f, Options{})
	err := c.waitHubReady(context.Background())
	if err == nil {
		t.Fatal("expected timeout error when hub never comes up")
	}
	if !strings.Contains(err.Error(), "hub not ready") {
		t.Fatalf("error should mention hub not ready: %q", err.Error())
	}
}

// TestWaitRuntimeBrokerReadyReturnsWhenOnline: an online broker in the listing
// resolves the gate immediately (one hub call, no error).
func TestWaitRuntimeBrokerReadyReturnsWhenOnline(t *testing.T) {
	save := brokerReadyInterval
	defer func() { brokerReadyInterval = save }()
	brokerReadyInterval = 0

	f := exec.NewFakeRunner()
	f.Script("scion hub brokers", exec.Result{Stdout: `[{"status":"online","connectionState":"connected"}]`})
	c := New(f, Options{})
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
	save := brokerReadyInterval
	defer func() { brokerReadyInterval = save }()
	brokerReadyInterval = 0

	f := exec.NewFakeRunner()
	f.Script("scion hub brokers", exec.Result{
		Stdout: "WARNING: development auth is enabled — do not use in production\n[{\"status\":\"online\",\"connectionState\":\"connected\"}]",
	})
	c := New(f, Options{})
	if err := c.WaitRuntimeBrokerReady(context.Background(), "/lever"); err != nil {
		t.Fatalf("gate must see the broker through the dev-auth banner: %v", err)
	}
	if len(f.Calls) != 1 {
		t.Errorf("hub-brokers calls = %d, want 1 (banner stripped, broker seen online)", len(f.Calls))
	}
}

// TestWaitRuntimeBrokerReadyOfflineIsNotReadyThenFailSoft: a broker that is
// registered but NOT online must not satisfy the gate — it keeps polling the
// whole budget — and on exhaustion the gate is fail-soft (returns nil, never an
// error, so it can't fail the bring-up; the start-path retry backstops).
func TestWaitRuntimeBrokerReadyOfflineIsNotReadyThenFailSoft(t *testing.T) {
	saveAtt, saveInt := brokerReadyAttempts, brokerReadyInterval
	defer func() { brokerReadyAttempts, brokerReadyInterval = saveAtt, saveInt }()
	brokerReadyAttempts, brokerReadyInterval = 3, 0

	f := exec.NewFakeRunner()
	f.Script("scion hub brokers", exec.Result{Stdout: `[{"status":"offline","connectionState":"disconnected"}]`})
	c := New(f, Options{})
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
	saveAtt, saveInt := brokerReadyAttempts, brokerReadyInterval
	defer func() { brokerReadyAttempts, brokerReadyInterval = saveAtt, saveInt }()
	brokerReadyAttempts, brokerReadyInterval = 30, time.Hour

	f := exec.NewFakeRunner()
	f.Script("scion hub brokers", exec.Result{Stdout: `[]`}) // never ready
	c := New(f, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.WaitRuntimeBrokerReady(ctx, "/lever"); err == nil {
		t.Fatal("a cancelled context must return an error, not fail-soft nil")
	}
}
