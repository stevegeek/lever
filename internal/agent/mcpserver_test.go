package agent

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/cap/token"
)

// fakeDirectiveBroker builds a plain (non-TLS) httptest server standing in
// for the broker's /directive/consume and /directive/check jail routes: it
// decodes the {"id":...} body and hands the path + id to handle, which
// writes the response. Mirrors the plain-http fake-broker pattern used for
// the CLI's broker calls (cli/msg_test.go's withFakeMsgBroker) — this is
// unit-level and doesn't exercise mTLS itself, unlike this file's other
// tests which use the real testBroker().
func fakeDirectiveBroker(t *testing.T, handle func(w http.ResponseWriter, path, id string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		handle(w, r.URL.Path, body.ID)
	}))
	t.Cleanup(srv.Close)
	return srv
}

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

func TestMCPToolsListAdvertisesDirectiveTools(t *testing.T) {
	s := NewMCPServer(MCPConfig{BrokerURL: "http://x", AgentCN: "manager"})
	resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	if !names["directive_consume"] || !names["directive_check"] {
		t.Fatalf("directive tools missing: %v", names)
	}
}

func TestMCPDirectiveConsumePostsIDAndSurfacesAction(t *testing.T) {
	var gotPath, gotID string
	srv := fakeDirectiveBroker(t, func(w http.ResponseWriter, path, id string) {
		gotPath, gotID = path, id
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "kind": "tool_call",
			"action": map[string]any{"tool": "db", "op": "read"},
		})
	})
	s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "manager", Client: srv.Client()})

	text := rpcText(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"directive_consume","arguments":{"id":"D1"}}}`)
	if gotPath != "/directive/consume" {
		t.Fatalf("path = %s, want /directive/consume", gotPath)
	}
	if gotID != "D1" {
		t.Fatalf("posted id = %q, want D1", gotID)
	}
	if !strings.Contains(text, `"kind":"tool_call"`) || !strings.Contains(text, `"tool":"db"`) {
		t.Fatalf("tool result text = %q, want the broker's action JSON surfaced verbatim", text)
	}
}

func TestMCPDirectiveCheckPostsIDAndSurfacesState(t *testing.T) {
	var gotPath, gotID string
	srv := fakeDirectiveBroker(t, func(w http.ResponseWriter, path, id string) {
		gotPath, gotID = path, id
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "state": "active"})
	})
	s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "manager", Client: srv.Client()})

	text := rpcText(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"directive_check","arguments":{"id":"D2"}}}`)
	if gotPath != "/directive/check" {
		t.Fatalf("path = %s, want /directive/check", gotPath)
	}
	if gotID != "D2" {
		t.Fatalf("posted id = %q, want D2", gotID)
	}
	if !strings.Contains(text, `"state":"active"`) {
		t.Fatalf("tool result text = %q, want the broker's state JSON surfaced verbatim", text)
	}
}

func TestMCPDirectiveConsume404SurfacesAsToolCallError(t *testing.T) {
	// The broker's opaque-404 contract (unknown id / wrong target / already
	// consumed / expired / stale generation — all byte-identical) must reach
	// the model as "not found" and nothing more, matching how the existing
	// `request` case surfaces a broker error (mcpserver.go's writeRPCError),
	// never a transport panic.
	srv := fakeDirectiveBroker(t, func(w http.ResponseWriter, _, _ string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})
	s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "manager", Client: srv.Client()})

	resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"directive_consume","arguments":{"id":"nope"}}}`)
	e, isErr := resp["error"].(map[string]any)
	if !isErr {
		t.Fatalf("404 broker reply must surface as a JSON-RPC error, got %v", resp)
	}
	msg, _ := e["message"].(string)
	if !strings.Contains(msg, "not found") {
		t.Fatalf("error message = %q, want it to contain %q", msg, "not found")
	}
}

func TestMCPDirectiveAcceptsDirectiveIDAlias(t *testing.T) {
	// Every identifier the model has ever seen for a directive is spelled
	// `directive_id` — the signed statement's field, `lever directive send`'s
	// output, the design docs. Calling the tool with that spelling must work
	// rather than silently posting an empty id (which the broker rejects as a
	// bad body and answers with the opaque 404, so the model concludes the
	// operator's directive does not exist).
	for _, tc := range []struct{ tool, route string }{
		{"directive_consume", "/directive/consume"},
		{"directive_check", "/directive/check"},
	} {
		var gotPath, gotID string
		srv := fakeDirectiveBroker(t, func(w http.ResponseWriter, path, id string) {
			gotPath, gotID = path, id
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "state": "active"})
		})
		s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "manager", Client: srv.Client()})

		rpcText(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tc.tool+`","arguments":{"directive_id":"D9"}}}`)
		if gotPath != tc.route || gotID != "D9" {
			t.Fatalf("%s with directive_id alias posted (%q, %q), want (%q, %q)", tc.tool, gotPath, gotID, tc.route, "D9")
		}
	}
}

func TestMCPDirectiveMissingIDIsALocalArgumentError(t *testing.T) {
	// A missing id is the CALLER's mistake, not a directive-state fact, so it
	// must fail locally with an actionable message and never reach the broker.
	// Letting it through would return the opaque "not found" — indistinguishable
	// from "no such directive", which teaches the agent to disbelieve a genuine
	// operator authorization.
	for _, tool := range []string{"directive_consume", "directive_check"} {
		called := false
		srv := fakeDirectiveBroker(t, func(w http.ResponseWriter, _, _ string) { called = true })
		s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "manager", Client: srv.Client()})

		resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":{}}}`)
		e, isErr := resp["error"].(map[string]any)
		if !isErr {
			t.Fatalf("%s without an id must return a JSON-RPC error, got %v", tool, resp)
		}
		// The CODE is half the fix: -32602 (invalid params) is what marks this
		// as the caller's mistake rather than a broker verdict, so assert it —
		// a silent revert to -32000 must fail here.
		if code, _ := e["code"].(float64); int(code) != -32602 {
			t.Fatalf("%s missing-id error code = %v, want -32602 (invalid params)", tool, e["code"])
		}
		msg, _ := e["message"].(string)
		if !strings.Contains(msg, "missing or non-string required argument") || strings.Contains(msg, "not found") {
			t.Fatalf("%s missing-id error = %q, want it to name the argument and NOT read as a directive miss", tool, msg)
		}
		if called {
			t.Fatalf("%s without an id must not reach the broker", tool)
		}
	}
}

func TestMCPDirectiveIDGuardsFailLocally(t *testing.T) {
	// Each of these is a caller-side mistake that must NOT be posted to the
	// broker: posting it would return the opaque 404 and teach the agent that a
	// real operator directive does not exist — the whole bug this guards.
	for _, tc := range []struct{ name, args string }{
		{"whitespace only", `{"id":"   "}`},
		{"conflicting spellings", `{"id":"D1","directive_id":"D2"}`},
		{"absurdly long", `{"id":"` + strings.Repeat("x", 129) + `"}`},
		{"non-string id", `{"id":12345}`},
	} {
		called := false
		srv := fakeDirectiveBroker(t, func(w http.ResponseWriter, _, _ string) { called = true })
		s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "manager", Client: srv.Client()})

		resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"directive_consume","arguments":`+tc.args+`}}`)
		e, isErr := resp["error"].(map[string]any)
		if !isErr {
			t.Fatalf("%s: want a JSON-RPC error, got %v", tc.name, resp)
		}
		if code, _ := e["code"].(float64); int(code) != -32602 {
			t.Fatalf("%s: error code = %v, want -32602", tc.name, e["code"])
		}
		if called {
			t.Fatalf("%s: must not reach the broker", tc.name)
		}
	}

	// Surrounding whitespace is trimmed, not rejected: a copy-pasted id with a
	// trailing newline is a valid id, and failing it would recreate the bug.
	var gotID string
	srv := fakeDirectiveBroker(t, func(w http.ResponseWriter, _, id string) {
		gotID = id
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "kind": "instruction"})
	})
	s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "manager", Client: srv.Client()})
	rpcText(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"directive_consume","arguments":{"id":"  D1\n"}}}`)
	if gotID != "D1" {
		t.Fatalf("posted id = %q, want the trimmed %q", gotID, "D1")
	}
}

func TestMCPDirectiveSchemaDeclaresBothSpellings(t *testing.T) {
	// The advertised contract must match what the server accepts: declaring
	// only `id` while quietly tolerating `directive_id` leaves a
	// schema-validating client free to reject the alias before it reaches us.
	s := NewMCPServer(MCPConfig{BrokerURL: "http://x", AgentCN: "manager"})
	resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	for _, tl := range resp["result"].(map[string]any)["tools"].([]any) {
		tool := tl.(map[string]any)
		name := tool["name"].(string)
		if name != "directive_consume" && name != "directive_check" {
			continue
		}
		schema := tool["inputSchema"].(map[string]any)
		props := schema["properties"].(map[string]any)
		if props["id"] == nil || props["directive_id"] == nil {
			t.Fatalf("%s schema properties = %v, want both id and directive_id declared", name, props)
		}
		if schema["anyOf"] == nil {
			t.Fatalf("%s schema must require one of the two spellings via anyOf, got %v", name, schema)
		}
		if schema["required"] != nil {
			t.Fatalf("%s schema still carries a flat required (%v), which contradicts the anyOf", name, schema["required"])
		}
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
