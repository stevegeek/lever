package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/wire"
)

// TestMCPChatVerifyPostsTheEnvelopeFields: chat_verify posts timestamp and
// from to /chat/verify and surfaces the broker's answer verbatim.
func TestMCPChatVerifyPostsTheEnvelopeFields(t *testing.T) {
	var got wire.ChatVerifyRequest
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(wire.ChatVerifyResponse{Enabled: true, Verified: true,
			Messages: []wire.VerifiedMessage{{Login: "op@example.com", Tier: "operator", Text: "deploy the fix"}}})
	}))
	t.Cleanup(srv.Close)
	s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "manager", Client: srv.Client()})

	text := rpcText(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"chat_verify","arguments":{"timestamp":"2026-09-28T10:15:02Z","from":"user:op@example.com"}}}`)
	if path != wire.PathChatVerify || got.Timestamp != "2026-09-28T10:15:02Z" || got.From != "user:op@example.com" {
		t.Fatalf("posted %s %+v", path, got)
	}
	if !strings.Contains(text, `"verified":true`) || !strings.Contains(text, "deploy the fix") {
		t.Fatalf("tool result = %q, want the broker's answer", text)
	}
}

// TestMCPChatVerifyNeedsBothFields: a missing field is a local argument
// error (-32602), never a broker call that could read as "not verified".
func TestMCPChatVerifyNeedsBothFields(t *testing.T) {
	s := NewMCPServer(MCPConfig{BrokerURL: "http://127.0.0.1:1", AgentCN: "manager"})
	for _, args := range []string{`{"timestamp":"2026-09-28T10:15:02Z"}`, `{"from":"user:op"}`, `{}`} {
		resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"chat_verify","arguments":`+args+`}}`)
		e, ok := resp["error"].(map[string]any)
		if !ok || e["code"].(float64) != -32602 {
			t.Fatalf("args %s: response %v, want a -32602 error", args, resp)
		}
	}
}

func TestMCPToolsListAdvertisesChatVerify(t *testing.T) {
	s := NewMCPServer(MCPConfig{BrokerURL: "http://x", AgentCN: "manager"})
	resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	for _, tl := range resp["result"].(map[string]any)["tools"].([]any) {
		m := tl.(map[string]any)
		if m["name"] == "chat_verify" {
			schema := m["inputSchema"].(map[string]any)
			for _, bad := range []string{"anyOf", "oneOf", "allOf"} {
				if _, has := schema[bad]; has {
					t.Fatalf("chat_verify schema has a top-level %s (Claude Code drops such tools, #24)", bad)
				}
			}
			return
		}
	}
	t.Fatal("chat_verify is not listed")
}
