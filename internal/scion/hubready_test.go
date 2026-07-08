package scion

import (
	"context"
	"strings"
	"testing"

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
	if err := c.ServerStart(context.Background(), ServerOpts{Port: 8080, DevAuth: false}); err != nil {
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
