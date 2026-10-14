package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/cap/token"
)

// fakeDirectiveBroker is a plain-HTTP stand-in for the broker's directive
// routes; it does not exercise mTLS.
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

func rpcText(t *testing.T, s *MCPServer, body string) string {
	t.Helper()
	resp := rpc(t, s, body)
	if e, isErr := resp["error"]; isErr {
		t.Fatalf("tools/call errored: %v", e)
	}
	content := resp["result"].(map[string]any)["content"].([]any)
	return content[0].(map[string]any)["text"].(string)
}

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
	var resp map[string]any
	_ = json.Unmarshal(s.Handle(context.Background(), []byte(body)), &resp)
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
	// Bound to the recipient, not the delegator.
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
	// With no bound_to, request binds to the caller, and extra args become
	// narrowing constraints in the token.
	env := testBroker(t)
	env.Rules.AllowObtain("manager", "db", "read")
	regDB(t, env)
	managerID := enrolManager(t, env.CA)
	client, _ := managerID.Client()
	s := NewMCPServer(MCPConfig{BrokerURL: env.Server.URL, AgentCN: "manager", Client: client})

	text := rpcText(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"request","arguments":{"tool":"db","op":"read","table":"A"}}}`)
	pub := env.Keys.Public
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
	if !names["directive_consume"] || !names["directive_check"] || !names["directive_preview"] {
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

func TestMCPDirectivePreviewPostsIDAndSurfacesPreviewVerbatim(t *testing.T) {
	var gotPath, gotID string
	srv := fakeDirectiveBroker(t, func(w http.ResponseWriter, path, id string) {
		gotPath, gotID = path, id
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "kind": "tool_call", "consumed": false,
			"preview": map[string]any{"tool": "db", "op": "read"}, "note": "PREVIEW ONLY",
		})
	})
	s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "manager", Client: srv.Client()})

	text := rpcText(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"directive_preview","arguments":{"id":"D3"}}}`)
	if gotPath != "/directive/preview" || gotID != "D3" {
		t.Fatalf("posted (%q, %q), want (/directive/preview, D3)", gotPath, gotID)
	}
	for _, want := range []string{`"consumed":false`, `"preview":{`, `PREVIEW ONLY`} {
		if !strings.Contains(text, want) {
			t.Fatalf("tool result text = %q, want it to contain %s", text, want)
		}
	}
}

// The tool description is what tells the model a preview is not a consume.
func TestMCPDirectivePreviewDescriptionSaysNotAuthority(t *testing.T) {
	s := NewMCPServer(MCPConfig{BrokerURL: "http://x", AgentCN: "manager"})
	resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	for _, tl := range resp["result"].(map[string]any)["tools"].([]any) {
		tool := tl.(map[string]any)
		if tool["name"] != "directive_preview" {
			continue
		}
		desc := tool["description"].(string)
		for _, want := range []string{"WITHOUT consuming", "not operator authority", "directive_consume"} {
			if !strings.Contains(desc, want) {
				t.Fatalf("directive_preview description lacks %q: %s", want, desc)
			}
		}
		return
	}
	t.Fatal("directive_preview not advertised")
}

func TestMCPDirectiveConsume404SurfacesAsToolCallError(t *testing.T) {
	// The broker's opaque 404 must reach the model as "not found" and nothing
	// more.
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
	// The model sees directive_id everywhere, so that spelling must work
	// rather than post an empty id and get the opaque 404.
	for _, tc := range []struct{ tool, route string }{
		{"directive_consume", "/directive/consume"},
		{"directive_check", "/directive/check"},
		{"directive_preview", "/directive/preview"},
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
	// A missing id must fail locally: the broker's opaque "not found" would
	// teach the agent to disbelieve a genuine operator authorization.
	for _, tool := range []string{"directive_consume", "directive_check", "directive_preview"} {
		called := false
		srv := fakeDirectiveBroker(t, func(w http.ResponseWriter, _, _ string) { called = true })
		s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "manager", Client: srv.Client()})

		resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":{}}}`)
		e, isErr := resp["error"].(map[string]any)
		if !isErr {
			t.Fatalf("%s without an id must return a JSON-RPC error, got %v", tool, resp)
		}
		// -32602 marks the caller's mistake rather than a broker verdict.
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
	// Caller-side mistakes that must not be posted to the broker.
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
	// trailing newline is valid.
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
	// Both spellings must appear in properties, and the schema must be a plain
	// top-level object with no combinator and no top-level required (see
	// directiveInputSchema).
	s := NewMCPServer(MCPConfig{BrokerURL: "http://x", AgentCN: "manager"})
	resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	for _, tl := range resp["result"].(map[string]any)["tools"].([]any) {
		tool := tl.(map[string]any)
		name := tool["name"].(string)
		if name != "directive_consume" && name != "directive_check" && name != "directive_preview" {
			continue
		}
		schema := tool["inputSchema"].(map[string]any)
		if schema["type"] != "object" {
			t.Fatalf("%s schema type = %v, want object", name, schema["type"])
		}
		props := schema["properties"].(map[string]any)
		if props["id"] == nil || props["directive_id"] == nil {
			t.Fatalf("%s schema properties = %v, want both id and directive_id declared", name, props)
		}
		for _, combinator := range []string{"anyOf", "oneOf", "allOf", "required"} {
			if schema[combinator] != nil {
				t.Fatalf("%s schema carries top-level %q (%v) — the Anthropic API rejects it and Claude Code silently drops the tool (#24)", name, combinator, schema[combinator])
			}
		}
	}
}

// fakeCapBroker records whether the broker was reached at all: for a guard
// that must fail caller-side, reaching it is the bug.
func fakeCapBroker(t *testing.T, called *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// rejectedLocally pins the caller-side rejection shape: -32602 and the broker
// never reached. It returns the error message.
func rejectedLocally(t *testing.T, label, tool, args string) string {
	t.Helper()
	called := false
	srv := fakeCapBroker(t, &called)
	s := NewMCPServer(MCPConfig{BrokerURL: srv.URL, AgentCN: "manager", Client: srv.Client()})

	resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+args+`}}`)
	e, isErr := resp["error"].(map[string]any)
	if !isErr {
		t.Fatalf("%s: must error, got %v", label, resp)
	}
	if code, _ := e["code"].(float64); int(code) != -32602 {
		t.Fatalf("%s: error code = %v, want -32602 (invalid params)", label, e["code"])
	}
	if called {
		t.Fatalf("%s: must not reach the broker", label)
	}
	msg, _ := e["message"].(string)
	return msg
}

func TestMCPDelegateWithoutRecipientIsALocalArgumentError(t *testing.T) {
	// An absent, blank or misspelt to would reach the broker as an empty bind
	// target, which the broker defaults to the caller: a silent self-bound
	// token.
	for _, tc := range []struct{ name, args string }{
		{"absent", `{"tool":"db","op":"read"}`},
		{"blank", `{"tool":"db","op":"read","to":""}`},
		{"whitespace only", `{"tool":"db","op":"read","to":"   "}`},
		// constraintArgs would not strip agent, making it a bogus constraint.
		{"misspelt as agent", `{"tool":"db","op":"read","agent":"worker"}`},
		// Non-string values are dropped by handleToolsCall, so this arrives absent.
		{"non-string", `{"tool":"db","op":"read","to":123}`},
		// Only the sibling tool's spelling, which delegate does not declare.
		{"sibling spelling only", `{"tool":"db","op":"read","bound_to":"worker"}`},
	} {
		msg := rejectedLocally(t, "delegate without a recipient ("+tc.name+")", "delegate", tc.args)
		if !strings.Contains(msg, `"to"`) {
			t.Fatalf("%s: error = %q, want it to name the missing argument", tc.name, msg)
		}
	}
}

func TestMCPCapabilityToolsRequireToolAndOp(t *testing.T) {
	// A blank tool/op must fail locally, not as a misattributed broker
	// verdict.
	for _, tc := range []struct{ tool, args, want string }{
		{"request", `{"op":"read"}`, `"tool"`},
		{"request", `{"tool":"","op":"read"}`, `"tool"`},
		{"request", `{"tool":"db"}`, `"op"`},
		{"request", `{"tool":"db","op":"  "}`, `"op"`},
		{"delegate", `{"op":"read","to":"worker"}`, `"tool"`},
		{"delegate", `{"tool":"db","to":"worker"}`, `"op"`},
	} {
		msg := rejectedLocally(t, tc.tool+" "+tc.args, tc.tool, tc.args)
		if !strings.Contains(msg, tc.want) {
			t.Fatalf("%s %s: error = %q, want it to name %s", tc.tool, tc.args, msg, tc.want)
		}
	}
}

func TestMCPDelegateRejectsConflictingBindTarget(t *testing.T) {
	// A bound_to that disagrees with to is refused rather than picked.
	msg := rejectedLocally(t, "conflicting bind target", "delegate",
		`{"tool":"db","op":"read","to":"worker","bound_to":"other"}`)
	if !strings.Contains(msg, `"to"`) {
		t.Fatalf("error = %q, want it to point at the canonical argument", msg)
	}
}

func TestMCPDelegateAcceptsAgreeingBindTarget(t *testing.T) {
	// A bound_to that agrees with to is accepted, as directiveID accepts
	// agreeing duplicates.
	env := testBroker(t)
	allowDelegate(t, env, "manager", "db", "read", "worker")
	regDB(t, env)
	managerID := enrolManager(t, env.CA)
	client, _ := managerID.Client()
	s := NewMCPServer(MCPConfig{BrokerURL: env.Server.URL, AgentCN: "manager", Client: client})

	text := rpcText(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delegate","arguments":{"tool":"db","op":"read","to":"worker","bound_to":"worker"}}}`)
	if !verifies(env.Keys.Public, text, "worker", map[string]string{}) {
		t.Fatal("token must be bound to the agreed recipient (worker)")
	}
	// The redundant spelling must not survive as a narrowing constraint: baking
	// bound_to=worker into the token would fail closed at call time.
	if !verifies(env.Keys.Public, text, "worker", map[string]string{}) {
		t.Fatal("bound_to must be reserved, not baked in as a constraint")
	}
}

func TestMCPDelegateRejectsSelfAsRecipient(t *testing.T) {
	// Self-delegation would bypass the delegate policy (MayObtainRule consults
	// the obtain set) and audit as a self-obtain.
	msg := rejectedLocally(t, "self-delegation", "delegate", `{"tool":"db","op":"read","to":"manager"}`)
	if !strings.Contains(msg, "request") {
		t.Fatalf("error = %q, want it to point at the tool that does mint for self", msg)
	}
}

func TestMCPRequestRejectsSiblingBindArgumentWithoutMisinforming(t *testing.T) {
	// A stray to on request would otherwise become a constraint on a
	// self-bound token (see requestArgs).
	msg := rejectedLocally(t, "request with a stray `to` (must not self-bind)", "request",
		`{"tool":"db","op":"read","to":"worker"}`)
	// "unknown" would invite dropping the argument, which mints a wider
	// token.
	if strings.Contains(msg, "unknown") {
		t.Fatalf("error = %q, must not claim the argument is unknown", msg)
	}
	if !strings.Contains(msg, `"bound_to"`) || !strings.Contains(msg, "delegate") {
		t.Fatalf("error = %q, want it to name both readings (bound_to, or the delegate tool)", msg)
	}
}

func TestMCPUnknownToolDenied(t *testing.T) {
	// An unrecognised tool name gets method-not-found, never a mint.
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
	// No response path may leak key material: the MCP handler is driven by an
	// in-jail LLM.
	s := NewMCPServer(MCPConfig{BrokerURL: "http://x", AgentCN: "manager"})

	resp := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	raw, _ := json.Marshal(resp)
	if strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("initialize response must never contain key material")
	}

	resp = rpc(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	raw, _ = json.Marshal(resp)
	if strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("tools/list response must never contain key material")
	}

	resp = rpc(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"unknown_tool","arguments":{}}}`)
	raw, _ = json.Marshal(resp)
	if strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("tools/call error response must never contain key material")
	}
}
