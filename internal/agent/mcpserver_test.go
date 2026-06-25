package agent

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func rpc(t *testing.T, s *MCPServer, body string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/", strings.NewReader(body)))
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return resp
}

func TestMCPToolsListAdvertisesCapabilityTools(t *testing.T) {
	s := NewMCPServer(MCPConfig{BrokerURL: "http://x", AgentCN: "manager"})
	resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	if !names["request"] || !names["attenuate"] || !names["delegate"] {
		t.Fatalf("capability tools missing: %v", names)
	}
}

func TestMCPDelegateMintsAndAttenuates(t *testing.T) {
	env := testBroker(t)
	allowDelegate(t, env, "manager", "db", "read", "worker")
	regDB(t, env)
	managerID := enrolManager(t, env.CA)
	client, _ := managerID.Client()
	s := NewMCPServer(MCPConfig{BrokerURL: env.Server.URL, AgentCN: "manager", Client: client})
	resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delegate","arguments":{"tool":"db","op":"read","to":"worker","table":"A","filter":"alice"}}}`)
	if _, isErr := resp["error"]; isErr {
		t.Fatalf("delegate errored: %s", resp["error"])
	}
	// The result content carries a non-empty token string.
	content := resp["result"].(map[string]any)["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if text == "" {
		t.Fatal("delegate returned an empty token")
	}
	// The token must be valid base64url (biscuit tokens are base64url-encoded).
	if _, err := base64.RawURLEncoding.DecodeString(text); err != nil {
		t.Fatalf("delegate token is not valid base64url: %v", err)
	}
}

func TestMCPServerNeverExposesKey(t *testing.T) {
	// No response path (initialize, tools/list, or error from tools/call) may leak
	// key material. This matters because the MCP handler is driven by an in-jail LLM.
	s := NewMCPServer(MCPConfig{BrokerURL: "http://x", AgentCN: "manager"})

	// 1. initialize
	resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	raw, _ := json.Marshal(resp)
	if strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("initialize response must never contain key material")
	}

	// 2. tools/list
	resp = rpc(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	raw, _ = json.Marshal(resp)
	if strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("tools/list response must never contain key material")
	}

	// 3. tools/call error path (no broker Client configured, so the call will error).
	resp = rpc(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"unknown_tool","arguments":{}}}`)
	raw, _ = json.Marshal(resp)
	if strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("tools/call error response must never contain key material")
	}
}
