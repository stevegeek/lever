package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestUnknownSubcommandErrors(t *testing.T) {
	if err := run([]string{"lever-agent", "bogus"}); err == nil {
		t.Fatal("unknown subcommand must error")
	}
}

func TestRunRequiresSubcommand(t *testing.T) {
	if err := run([]string{"lever-agent"}); err == nil {
		t.Fatal("missing subcommand must error")
	}
}

// TestBuildToolCallBody verifies that the JSON-RPC body produced for the
// gateway satisfies the contract expected by internal/broker/mcp.go:toolsCallFields:
//   - jsonrpc == "2.0", method == "tools/call"
//   - params.name == op
//   - params.arguments._capability == token
//   - extra kv args appear in params.arguments
func TestBuildToolCallBody(t *testing.T) {
	const op = "query"
	const tok = "tok_abc123"
	extra := map[string]string{"table": "users", "limit": "10"}

	body := buildToolCallBody(op, tok, extra)

	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := msg["jsonrpc"]; got != "2.0" {
		t.Errorf("jsonrpc: got %v, want 2.0", got)
	}
	if got := msg["method"]; got != "tools/call" {
		t.Errorf("method: got %v, want tools/call", got)
	}

	params, ok := msg["params"].(map[string]any)
	if !ok {
		t.Fatal("params missing or wrong type")
	}
	if got := params["name"]; got != op {
		t.Errorf("params.name: got %v, want %q", got, op)
	}

	args, ok := params["arguments"].(map[string]any)
	if !ok {
		t.Fatal("params.arguments missing or wrong type")
	}
	if got := args["_capability"]; got != tok {
		t.Errorf("arguments._capability: got %v, want %q", got, tok)
	}
	if got := args["table"]; got != "users" {
		t.Errorf("arguments.table: got %v, want users", got)
	}
	if got := args["limit"]; got != "10" {
		t.Errorf("arguments.limit: got %v, want 10", got)
	}
}

// TestRenewFlagAcceptance verifies that the renew flagset accepts --loop and
// --interval without a parse error (reconciles manifest.json sidecar declaration).
func TestRenewFlagAcceptance(t *testing.T) {
	fs := flag.NewFlagSet("renew", flag.ContinueOnError)
	defaultIDDir := filepath.Join(os.Getenv("HOME"), ".lever-id")
	fs.String("id-dir", defaultIDDir, "")
	fs.String("broker-url", "", "")
	fs.String("bootstrap", "", "")
	loop := fs.Bool("loop", false, "")
	interval := fs.Duration("interval", 12*time.Hour, "")

	if err := fs.Parse([]string{"--loop", "--interval", "6h"}); err != nil {
		t.Fatalf("flag parse error (manifest sidecar would crash): %v", err)
	}
	if !*loop {
		t.Error("--loop should be true after parse")
	}
	if *interval != 6*time.Hour {
		t.Errorf("--interval: got %v, want 6h", *interval)
	}
}

// TestRenewOnceNoIdentityErrors verifies that renewOnce returns an error (not a
// panic or hang) when no identity exists in the directory.
func TestRenewOnceNoIdentityErrors(t *testing.T) {
	tmp := t.TempDir()
	err := renewOnce(renewOpts{idDir: tmp})
	if err == nil {
		t.Fatal("renewOnce with empty dir must return an error")
	}
	if !strings.Contains(err.Error(), "no identity") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRenewNonLoopReturnsErrorImmediately verifies that run with renew (no
// --loop) returns immediately with an error for an empty id-dir (no hang).
func TestRenewNonLoopReturnsErrorImmediately(t *testing.T) {
	tmp := t.TempDir()
	err := run([]string{"lever-agent", "renew", "--id-dir", tmp})
	if err == nil {
		t.Fatal("renew with no identity must error")
	}
	if !strings.Contains(err.Error(), "no identity") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestRenewLoopFlagsAcceptedByRealCmd exercises the REAL dispatch path through
// run() to prove that cmdRenew accepts --loop and --interval without a
// "flag provided but not defined" parse error. Loop mode only exits on
// SIGINT/SIGTERM, so we send SIGINT to ourselves after a brief delay to unblock
// it. The test asserts that any returned error is NOT a flag-parse error (an
// "no identity" or nil return both indicate the flags were accepted).
func TestRenewLoopFlagsAcceptedByRealCmd(t *testing.T) {
	tmp := t.TempDir()

	// Send SIGINT to ourselves after 50ms to unblock the loop.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()

	err := run([]string{"lever-agent", "renew", "--id-dir", tmp, "--loop", "--interval", "24h"})
	// Loop mode exits cleanly (nil) on signal. Either way, the error must NOT be
	// a flag-parse error — that would mean cmdRenew doesn't define --loop/--interval.
	if err != nil && (strings.Contains(err.Error(), "flag provided but not defined") ||
		strings.Contains(err.Error(), "flag: help requested")) {
		t.Fatalf("real cmdRenew rejected --loop/--interval (manifest sidecar would break): %v", err)
	}
}

// TestProvisionVerbAcceptedByRun verifies that run() dispatches "provision" and
// that the provision flags parse correctly. It uses a temp dir as -id-dir so there
// is no identity — cmdProvision errors with "no identity", which proves dispatch
// and flag parsing succeeded without a "flag provided but not defined" error.
func TestProvisionVerbAcceptedByRun(t *testing.T) {
	err := run([]string{"lever-agent", "provision", "-grove", "worker", "-out", t.TempDir() + "/w.json", "-id-dir", t.TempDir()})
	if err == nil {
		t.Fatal("expected an error (no identity), got nil")
	}
	if strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("provision flags must parse: %v", err)
	}
}

// TestBuildToolCallBodyEmptyArgs verifies token-only calls (no extra kv pairs).
func TestBuildToolCallBodyEmptyArgs(t *testing.T) {
	body := buildToolCallBody("op", "mytoken", nil)
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	params := msg["params"].(map[string]any)
	args := params["arguments"].(map[string]any)
	if got := args["_capability"]; got != "mytoken" {
		t.Errorf("arguments._capability: got %v, want mytoken", got)
	}
	// Only _capability should be present
	if len(args) != 1 {
		t.Errorf("expected 1 argument (only _capability), got %d: %v", len(args), args)
	}
}
