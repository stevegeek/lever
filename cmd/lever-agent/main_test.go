package main

import (
	"encoding/json"
	"testing"
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
