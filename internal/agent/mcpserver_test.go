package agent

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/cap/token"
)

// rpcText drives a tools/call and returns the result token text, failing if the
// call returned a JSON-RPC error.
func rpcText(t *testing.T, s *MCPServer, body string) string {
	t.Helper()
	resp := rpc(t, s, body)
	if e, isErr := resp["error"]; isErr {
		t.Fatalf("tools/call errored: %v", e)
	}
	content := resp["result"].(map[string]any)["content"].([]any)
	return content[0].(map[string]any)["text"].(string)
}

// verifies reports whether tok verifies for caller with the given params.
func verifies(pub []byte, tokB64, caller string, params map[string]string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(tokB64)
	if err != nil {
		return false
	}
	return token.Verify(pub, raw, token.Request{
		Caller:     caller,
		Capability: token.Capability{Tool: "db", Operation: "read"},
		Params:     params,
		Now:        time.Now(),
		MinEpoch:   0,
	}) == nil
}

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
	if !names["request"] || !names["delegate"] {
		t.Fatalf("capability tools missing: %v", names)
	}
	if names["attenuate"] {
		t.Fatalf("offline attenuate verb must no longer be advertised: %v", names)
	}
}

func TestMCPDelegateMintsRecipientBoundConstrainedToken(t *testing.T) {
	env := testBroker(t)
	allowDelegate(t, env, "manager", "db", "read", "worker")
	regDB(t, env)
	managerID := enrolManager(t, env.CA)
	client, _ := managerID.Client()
	s := NewMCPServer(MCPConfig{BrokerURL: env.Server.URL, AgentCN: "manager", Client: client})
	text := rpcText(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delegate","arguments":{"tool":"db","op":"read","to":"worker","table":"A","filter":"alice"}}}`)
	if text == "" {
		t.Fatal("delegate returned an empty token")
	}
	pub := env.Keys.Public
	// Bound to the recipient (worker), NOT the delegator (manager).
	if !verifies(pub, text, "worker", map[string]string{"table": "A", "filter": "alice"}) {
		t.Fatal("delegated token must verify for the recipient (worker)")
	}
	if verifies(pub, text, "manager", map[string]string{"table": "A", "filter": "alice"}) {
		t.Fatal("delegated token must NOT verify for the delegator (manager)")
	}
	// Narrowed by the constraint kv: a request missing the bound params is denied.
	if verifies(pub, text, "worker", map[string]string{}) {
		t.Fatal("delegated token must require its table/filter constraints (narrowing lost)")
	}
}

func TestMCPRequestMintsSelfBoundConstrainedToken(t *testing.T) {
	// The LLM-facing `request` tool (mcpserver.go:121): with no bound_to it defaults
	// to the caller's own CN, and extra (non-reserved) kv args become narrowing
	// constraints baked into the minted token (constraintArgs strips tool/op/bound_to).
	env := testBroker(t)
	env.Rules.AllowObtain("manager", "db", "read")
	regDB(t, env)
	managerID := enrolManager(t, env.CA)
	client, _ := managerID.Client()
	s := NewMCPServer(MCPConfig{BrokerURL: env.Server.URL, AgentCN: "manager", Client: client})

	text := rpcText(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"request","arguments":{"tool":"db","op":"read","table":"A"}}}`)
	pub := env.Keys.Public
	// Self-bound (default bound_to == caller CN).
	if !verifies(pub, text, "manager", map[string]string{"table": "A"}) {
		t.Fatal("request token must verify self-bound for the caller (manager) with the constraint satisfied")
	}
	// The table=A constraint must be baked in: a request without it is denied.
	if verifies(pub, text, "manager", map[string]string{}) {
		t.Fatal("request token must carry the table=A constraint (constraintArgs failed to bake it)")
	}
}

func TestMCPUnknownToolDenied(t *testing.T) {
	// The default case (mcpserver.go:151) must reject an unrecognised tool name with
	// the JSON-RPC method-not-found code rather than silently minting anything.
	s := NewMCPServer(MCPConfig{BrokerURL: "http://x", AgentCN: "manager"})
	resp := rpc(t, s, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"bogus","arguments":{}}}`)
	e, isErr := resp["error"].(map[string]any)
	if !isErr {
		t.Fatalf("unknown tool must return a JSON-RPC error, got %v", resp)
	}
	if code, _ := e["code"].(float64); int(code) != -32601 {
		t.Fatalf("unknown tool error code = %v, want -32601", e["code"])
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
