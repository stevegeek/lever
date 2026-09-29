package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/wire"
)

// TestMCPMessageVerifyPostsTheEnvelopeFields: message_verify and its alias
// chat_verify post timestamp, from and ref to /message/verify and surface the
// broker's answer verbatim.
func TestMCPMessageVerifyPostsTheEnvelopeFields(t *testing.T) {
	for _, tool := range []string{"message_verify", "chat_verify"} {
		var got wire.MessageVerifyRequest
		var path string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&got)
			_ = json.NewEncoder(w).Encode(wire.MessageVerifyResponse{Result: wire.VerifyLever,
				Messages: []wire.VerifiedMessage{{Source: wire.VerifyLever, Kind: "worker:alpha", Text: "done"}}})
		}))
		s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "manager", Client: srv.Client()})
		text := rpcText(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":{"timestamp":"2026-09-28T10:15:02Z","from":"user:dev@localhost","ref":"0123456789abcdef0123456789abcdef"}}}`)
		srv.Close()
		if path != wire.PathMessageVerify || got.Timestamp != "2026-09-28T10:15:02Z" || got.From != "user:dev@localhost" ||
			got.Ref != "0123456789abcdef0123456789abcdef" {
			t.Fatalf("%s posted %s %+v", tool, path, got)
		}
		if !strings.Contains(text, `"result":"lever"`) || !strings.Contains(text, "worker:alpha") {
			t.Fatalf("%s result = %q, want the broker's answer", tool, text)
		}
	}
}

// TestMCPMessageVerifyNeedsBothFields: a missing field is a local argument
// error (-32602), never a broker call.
func TestMCPMessageVerifyNeedsBothFields(t *testing.T) {
	s := NewMCPServer(MCPConfig{BrokerURL: "http://127.0.0.1:1", AgentCN: "manager"})
	for _, tool := range []string{"message_verify", "chat_verify"} {
		for _, args := range []string{`{"timestamp":"2026-09-28T10:15:02Z"}`, `{"from":"user:op"}`, `{"ref":"x"}`, `{}`} {
			resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+args+`}}`)
			e, ok := resp["error"].(map[string]any)
			if !ok || e["code"].(float64) != -32602 {
				t.Fatalf("%s args %s: response %v, want a -32602 error", tool, args, resp)
			}
		}
	}
}

// TestMCPToolsListAdvertisesMessageVerify: both names are listed, each with a
// plain object schema (a top-level combinator makes Claude Code drop the
// tool, #24) that offers ref.
func TestMCPToolsListAdvertisesMessageVerify(t *testing.T) {
	s := NewMCPServer(MCPConfig{BrokerURL: "http://x", AgentCN: "manager"})
	resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	seen := map[string]bool{}
	for _, tl := range resp["result"].(map[string]any)["tools"].([]any) {
		m := tl.(map[string]any)
		name, _ := m["name"].(string)
		if name != "message_verify" && name != "chat_verify" {
			continue
		}
		schema := m["inputSchema"].(map[string]any)
		if schema["type"] != "object" {
			t.Fatalf("%s schema is not an object", name)
		}
		for _, bad := range []string{"anyOf", "oneOf", "allOf"} {
			if _, has := schema[bad]; has {
				t.Fatalf("%s schema has a top-level %s (Claude Code drops such tools, #24)", name, bad)
			}
		}
		if _, ok := schema["properties"].(map[string]any)["ref"]; !ok {
			t.Fatalf("%s schema has no ref", name)
		}
		seen[name] = true
	}
	if !seen["message_verify"] || !seen["chat_verify"] {
		t.Fatalf("listed: %v, want message_verify and chat_verify", seen)
	}
}
