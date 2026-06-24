package broker

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseAndToolsCallFields(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"A","_capability":"TOK"}}}`)
	method, msg, ok := parseJSONRPC(body)
	if !ok || method != "tools/call" {
		t.Fatalf("parse: method=%q ok=%v", method, ok)
	}
	name, args, cap, ok := toolsCallFields(msg)
	if !ok || name != "read" || cap != "TOK" || args["table"] != "A" {
		t.Fatalf("fields: name=%q cap=%q args=%v ok=%v", name, cap, args, ok)
	}
	if _, present := args["_capability"]; present {
		t.Error("_capability must not appear in the extracted args map")
	}
}

func TestStripCapabilityRemovesItFromArguments(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"A","_capability":"TOK"}}}`)
	_, msg, _ := parseJSONRPC(body)
	out := stripCapability(msg)
	if strings.Contains(string(out), "_capability") {
		t.Fatalf("stripped body still contains _capability: %s", out)
	}
	if !strings.Contains(string(out), `"table":"A"`) {
		t.Fatalf("stripped body lost a real argument: %s", out)
	}
}

// --- Regression tests for FIX 1: canonical encoding of non-string args ---

// TestToolsCallFieldsObjectArgCanonicallyEncoded verifies that a non-string argument
// value (JSON object) is canonical-JSON-encoded, NOT coerced to "".
// This FAILS on the pre-fix code (which returns "" for any non-string) and must
// PASS after the fix.
func TestToolsCallFieldsObjectArgCanonicallyEncoded(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":{"$ne":null},"_capability":"TOK"}}}`)
	_, msg, ok := parseJSONRPC(body)
	if !ok {
		t.Fatal("parseJSONRPC failed")
	}
	name, args, cap, ok := toolsCallFields(msg)
	if !ok {
		t.Fatal("toolsCallFields returned ok=false")
	}
	if name != "read" {
		t.Fatalf("name = %q, want %q", name, "read")
	}
	if cap != "TOK" {
		t.Fatalf("cap = %q, want %q", cap, "TOK")
	}
	// The object must be canonical-JSON-encoded, NOT the empty string.
	got := args["table"]
	if got == "" {
		t.Fatal("SECURITY REGRESSION: non-string arg coerced to empty string (bypass still present)")
	}
	// Ensure it round-trips as the correct JSON value.
	want := `{"$ne":null}`
	if got != want {
		t.Fatalf("args[\"table\"] = %q, want %q", got, want)
	}
	if _, present := args["_capability"]; present {
		t.Error("_capability must not appear in the extracted args map")
	}
}

// TestToolsCallFieldsNumberArgCanonicallyEncoded verifies that a numeric argument
// is canonical-JSON-encoded (e.g. 10 → "10"), NOT coerced to "".
func TestToolsCallFieldsNumberArgCanonicallyEncoded(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"limit":10,"_capability":"TOK"}}}`)
	_, msg, ok := parseJSONRPC(body)
	if !ok {
		t.Fatal("parseJSONRPC failed")
	}
	_, args, cap, ok := toolsCallFields(msg)
	if !ok {
		t.Fatal("toolsCallFields returned ok=false")
	}
	if cap != "TOK" {
		t.Fatalf("cap = %q, want %q", cap, "TOK")
	}
	got := args["limit"]
	if got == "" {
		t.Fatal("SECURITY REGRESSION: numeric arg coerced to empty string (bypass still present)")
	}
	want := "10"
	if got != want {
		t.Fatalf("args[\"limit\"] = %q, want %q", got, want)
	}
}

// TestToolsCallFieldsStringArgPassesThroughRaw verifies that a plain string
// argument is still returned as-is (unchanged by the canonical encoding path).
func TestToolsCallFieldsStringArgPassesThroughRaw(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"A","_capability":"TOK"}}}`)
	_, msg, ok := parseJSONRPC(body)
	if !ok {
		t.Fatal("parseJSONRPC failed")
	}
	_, args, cap, ok := toolsCallFields(msg)
	if !ok {
		t.Fatal("toolsCallFields returned ok=false")
	}
	if cap != "TOK" {
		t.Fatalf("cap = %q, want %q", cap, "TOK")
	}
	got := args["table"]
	if got != "A" {
		t.Fatalf("args[\"table\"] = %q, want %q", got, "A")
	}
}

func TestAugmentToolsListSchemaInjectsCapability(t *testing.T) {
	resp := []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"read","inputSchema":{"type":"object","properties":{"table":{"type":"string"}}}}]}}`)
	out := augmentToolsListSchema(resp)
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	props := parsed["result"].(map[string]any)["tools"].([]any)[0].(map[string]any)["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["_capability"]; !ok {
		t.Fatalf("_capability not injected into schema: %s", out)
	}
	if _, ok := props["table"]; !ok {
		t.Error("existing schema property dropped")
	}
}
