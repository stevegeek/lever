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
