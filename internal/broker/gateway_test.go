package broker

import (
	"bytes"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/cap/token"
)

// upstreamMCP records whether it was reached and with what body.
func upstreamMCP(t *testing.T, reached *bool, gotBody *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		b, _ := io.ReadAll(r.Body)
		*gotBody = string(b)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}))
}

func mintFor(t *testing.T, b *Broker, agent string, cons map[string]string) string {
	t.Helper()
	c := make([]token.Constraint, 0, len(cons))
	for k, v := range cons {
		c = append(c, token.Constraint{Key: k, Value: v})
	}
	tok, err := token.Mint(b.keys.Private, token.Grant{
		Agent: agent, Capability: token.Capability{Tool: "db", Operation: "read"},
		Constraints: c, Expiry: time.Now().Add(time.Hour), Epoch: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	return base64urlNoPad(tok)
}

func TestGatewayAllowsValidCapabilityAndStripsIt(t *testing.T) {
	var reached bool
	var gotBody string
	up := upstreamMCP(t, &reached, &gotBody)
	defer up.Close()
	b := New(testConfig(t))
	_ = b.reg.Register(regTool("db", up.URL, "read"))

	cap := mintFor(t, b, "worker", nil)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"A","_capability":"` + cap + `"}}}`
	r := httptest.NewRequest("POST", "/mcp/db/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "worker")
	w := httptest.NewRecorder()
	h, err := b.gatewayHandler("db")
	if err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !reached {
		t.Fatal("backend should have been reached on a valid call")
	}
	if bytes.Contains([]byte(gotBody), []byte("_capability")) {
		t.Fatalf("token leaked upstream: %s", gotBody)
	}
}

func TestGatewayDeniesMissingCapabilityWithoutReachingBackend(t *testing.T) {
	var reached bool
	var gotBody string
	up := upstreamMCP(t, &reached, &gotBody)
	defer up.Close()
	b := New(testConfig(t))
	_ = b.reg.Register(regTool("db", up.URL, "read"))

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"A"}}}`
	r := httptest.NewRequest("POST", "/mcp/db/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "worker")
	w := httptest.NewRecorder()
	h, _ := b.gatewayHandler("db")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no _capability)", w.Code)
	}
	if reached {
		t.Fatal("SECURITY: backend reached despite missing capability")
	}
}

func TestGatewayDeniesWrongCallerWithoutReachingBackend(t *testing.T) {
	var reached bool
	var gotBody string
	up := upstreamMCP(t, &reached, &gotBody)
	defer up.Close()
	b := New(testConfig(t))
	_ = b.reg.Register(regTool("db", up.URL, "read"))

	cap := mintFor(t, b, "worker", nil) // bound to worker
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"A","_capability":"` + cap + `"}}}`
	r := httptest.NewRequest("POST", "/mcp/db/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "analyst") // caller is analyst, not worker
	w := httptest.NewRecorder()
	h, _ := b.gatewayHandler("db")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (caller != bound_agent)", w.Code)
	}
	if reached {
		t.Fatal("SECURITY: backend reached despite caller mismatch")
	}
}

func TestGatewayEnforcesConstraintAgainstArgs(t *testing.T) {
	var reached bool
	var gotBody string
	up := upstreamMCP(t, &reached, &gotBody)
	defer up.Close()
	b := New(testConfig(t))
	_ = b.reg.Register(regTool("db", up.URL, "read"))

	cap := mintFor(t, b, "worker", map[string]string{"table": "A"}) // constrained to table A
	// Request asks for table C -> must be denied.
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"C","_capability":"` + cap + `"}}}`
	r := httptest.NewRequest("POST", "/mcp/db/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "worker")
	w := httptest.NewRecorder()
	h, _ := b.gatewayHandler("db")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (constraint table==A, request table==C)", w.Code)
	}
	if reached {
		t.Fatal("SECURITY: backend reached despite constraint violation")
	}
}

// --- Regression tests for FIX 1 (gateway level): object arg must NOT satisfy empty-string constraint ---

// TestGatewayDeniesObjectArgAgainstEmptyStringConstraint is the end-to-end bypass
// regression. A token constrained to table="" must NOT be satisfied by table={"x":1}
// (or any non-string). Before the fix, toolsCallFields coerced {"x":1} to "" and the
// token.Verify passed — backend reached. After the fix, {"x":1} projects to `{"x":1}`
// which does not equal "", so token.Verify denies — backend NOT reached (403).
func TestGatewayDeniesObjectArgAgainstEmptyStringConstraint(t *testing.T) {
	var reached bool
	var gotBody string
	up := upstreamMCP(t, &reached, &gotBody)
	defer up.Close()
	b := New(testConfig(t))
	_ = b.reg.Register(regTool("db", up.URL, "read"))

	// Token constrained to table="" — should only allow a literal empty string.
	cap := mintFor(t, b, "worker", map[string]string{"table": ""})
	// Request sends table as a JSON object (injection attempt).
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":{"x":1},"_capability":"` + cap + `"}}}`
	r := httptest.NewRequest("POST", "/mcp/db/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "worker")
	w := httptest.NewRecorder()
	h, _ := b.gatewayHandler("db")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("SECURITY REGRESSION: status = %d, want 403 (object arg bypassed empty-string constraint)", w.Code)
	}
	if reached {
		t.Fatal("SECURITY REGRESSION: backend reached — object arg bypassed token.Verify constraint check")
	}
}

// --- Regression tests for FIX 2: method allowlist (fail-closed) ---

// TestGatewayDeniesUnknownMethodWithoutReachingBackend verifies that a JSON-RPC
// method outside the explicit allowlist is rejected 403 and the backend is NOT
// reached. Before the fix, the default: branch forwarded everything — fail-open.
func TestGatewayDeniesUnknownMethodWithoutReachingBackend(t *testing.T) {
	var reached bool
	var gotBody string
	up := upstreamMCP(t, &reached, &gotBody)
	defer up.Close()
	b := New(testConfig(t))
	_ = b.reg.Register(regTool("db", up.URL, "read"))

	body := `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{}}`
	r := httptest.NewRequest("POST", "/mcp/db/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "worker")
	w := httptest.NewRecorder()
	h, _ := b.gatewayHandler("db")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("SECURITY REGRESSION: status = %d, want 403 (unknown method must be denied)", w.Code)
	}
	if reached {
		t.Fatal("SECURITY REGRESSION: backend reached on unknown method (fail-open bypass)")
	}
}

// TestGatewayForwardsInitialize verifies that the allowlisted `initialize` method
// is forwarded to the backend unchanged (no capability required).
func TestGatewayForwardsInitialize(t *testing.T) {
	var reached bool
	var gotBody string
	up := upstreamMCP(t, &reached, &gotBody)
	defer up.Close()
	b := New(testConfig(t))
	_ = b.reg.Register(regTool("db", up.URL, "read"))

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}}}`
	r := httptest.NewRequest("POST", "/mcp/db/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "worker")
	w := httptest.NewRecorder()
	h, _ := b.gatewayHandler("db")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (initialize must be forwarded)", w.Code)
	}
	if !reached {
		t.Fatal("backend must be reached for initialize")
	}
}

// firstPartyTool registers a tool with FirstParty=true at the given backend.
func firstPartyTool(name, backend, op string) registry.Tool {
	t := regTool(name, backend, op)
	t.FirstParty = true
	return t
}

func TestGatewayFirstPartyForwardsTokenAndInjectsCaller(t *testing.T) {
	var reached bool
	var gotBody string
	var gotCaller string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotCaller = r.Header.Get("X-Lever-Caller")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}))
	defer up.Close()
	b := New(testConfig(t))
	_ = b.reg.Register(firstPartyTool("db", up.URL, "read"))

	cap := mintFor(t, b, "worker", nil)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"A","_capability":"` + cap + `"}}}`
	r := httptest.NewRequest("POST", "/mcp/db/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "worker")
	r.Header.Set("X-Lever-Caller", "manager") // FORGERY attempt — must be overwritten
	w := httptest.NewRecorder()
	h, _ := b.gatewayHandler("db")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !reached {
		t.Fatalf("status=%d reached=%v", w.Code, reached)
	}
	if !bytes.Contains([]byte(gotBody), []byte("_capability")) {
		t.Fatalf("first-party tool must receive the token; body=%s", gotBody)
	}
	if gotCaller != "worker" {
		t.Fatalf("X-Lever-Caller = %q, want worker (forged 'manager' must be overwritten)", gotCaller)
	}
}

// TestGatewayToolsListAugmentsSchema verifies that tools/list is forwarded and its
// response is augmented with _capability injection (ModifyResponse path).
func TestGatewayToolsListAugmentsSchema(t *testing.T) {
	var reached bool
	// The upstream returns a tools list.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"read","inputSchema":{"type":"object","properties":{"table":{"type":"string"}}}}]}}`)
	}))
	defer up.Close()

	b := New(testConfig(t))
	_ = b.reg.Register(regTool("db", up.URL, "read"))

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	r := httptest.NewRequest("POST", "/mcp/db/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "worker")
	w := httptest.NewRecorder()
	h, _ := b.gatewayHandler("db")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (tools/list must be forwarded)", w.Code)
	}
	if !reached {
		t.Fatal("backend must be reached for tools/list")
	}
	respBody := w.Body.String()
	if !bytes.Contains([]byte(respBody), []byte("_capability")) {
		t.Fatalf("_capability not injected into tools/list response schema: %s", respBody)
	}
}

// regCoarseTool registers an external coarse tool the way BuildBroker does:
// FirstParty=false, the synthetic wildcard op, Coarse+External set.
func regCoarseTool(name, backend string) registry.Tool {
	return registry.Tool{
		Name: name, Backend: backend, External: true, Coarse: true,
		Operations: map[string]registry.Operation{registry.WildcardOp: {Name: registry.WildcardOp}},
	}
}

func mintOp(t *testing.T, b *Broker, agent, tool, op string) string {
	t.Helper()
	tok, err := token.Mint(b.keys.Private, token.Grant{
		Agent: agent, Capability: token.Capability{Tool: tool, Operation: op},
		Expiry: time.Now().Add(time.Hour), Epoch: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	return base64urlNoPad(tok)
}

func TestGatewayCoarseToolAcceptsWildcardCapability(t *testing.T) {
	var reached bool
	var gotBody string
	up := upstreamMCP(t, &reached, &gotBody)
	defer up.Close()
	b := New(testConfig(t))
	_ = b.reg.Register(regCoarseTool("things3", up.URL))

	cap := mintOp(t, b, "worker", "things3", registry.WildcardOp)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add-todo","arguments":{"title":"x","_capability":"` + cap + `"}}}`
	r := httptest.NewRequest("POST", "/mcp/things3/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "worker")
	w := httptest.NewRecorder()
	h, err := b.gatewayHandler("things3")
	if err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !reached {
		t.Fatal("backend should be reached: wildcard capability on a coarse tool")
	}
	if bytes.Contains([]byte(gotBody), []byte("_capability")) {
		t.Fatalf("token leaked upstream: %s", gotBody)
	}
}

func TestGatewayCoarseToolDeniesPerOpCapability(t *testing.T) {
	var reached bool
	var gotBody string
	up := upstreamMCP(t, &reached, &gotBody)
	defer up.Close()
	b := New(testConfig(t))
	_ = b.reg.Register(regCoarseTool("things3", up.URL))

	// A token naming the specific MCP tool must NOT satisfy a coarse tool: the
	// gateway requires exactly {things3, "*"} there.
	cap := mintOp(t, b, "worker", "things3", "add-todo")
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add-todo","arguments":{"_capability":"` + cap + `"}}}`
	r := httptest.NewRequest("POST", "/mcp/things3/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "worker")
	w := httptest.NewRecorder()
	h, _ := b.gatewayHandler("things3")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if reached {
		t.Fatal("SECURITY: backend reached with a non-wildcard token on a coarse tool")
	}
}

func TestGatewayFineToolDeniesWildcardCapability(t *testing.T) {
	var reached bool
	var gotBody string
	up := upstreamMCP(t, &reached, &gotBody)
	defer up.Close()
	b := New(testConfig(t))
	_ = b.reg.Register(regTool("db", up.URL, "read")) // fine (Coarse=false)

	// A wildcard token must NOT satisfy a fine tool: the gateway requires the
	// real params.name there, and "db" has no "*" operation.
	cap := mintOp(t, b, "worker", "db", registry.WildcardOp)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"A","_capability":"` + cap + `"}}}`
	r := httptest.NewRequest("POST", "/mcp/db/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "worker")
	w := httptest.NewRecorder()
	h, _ := b.gatewayHandler("db")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (wildcard must not widen a fine tool)", w.Code)
	}
	if reached {
		t.Fatal("SECURITY: backend reached with a wildcard token on a fine tool")
	}
}

func TestGatewayCoarseCapabilityIsIdentityBound(t *testing.T) {
	var reached bool
	var gotBody string
	up := upstreamMCP(t, &reached, &gotBody)
	defer up.Close()
	b := New(testConfig(t))
	_ = b.reg.Register(regCoarseTool("things3", up.URL))

	cap := mintOp(t, b, "worker", "things3", registry.WildcardOp) // bound to worker
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add-todo","arguments":{"_capability":"` + cap + `"}}}`
	r := httptest.NewRequest("POST", "/mcp/things3/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "analyst") // replay by a different agent
	w := httptest.NewRecorder()
	h, _ := b.gatewayHandler("things3")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (coarse token replayed cross-agent)", w.Code)
	}
	if reached {
		t.Fatal("SECURITY: backend reached on a cross-agent coarse replay")
	}
}

func TestGatewayComposesBackendPath(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}))
	defer up.Close()
	b := New(testConfig(t))
	// qmd-style: the server mounts its MCP endpoint under /mcp; the config
	// backend carries the path, scheme-less (host:port/path).
	backend := strings.TrimPrefix(up.URL, "http://") + "/mcp"
	_ = b.reg.Register(regCoarseTool("qmd", backend))

	cap := mintOp(t, b, "worker", "qmd", registry.WildcardOp)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":{"_capability":"` + cap + `"}}}`
	r := httptest.NewRequest("POST", "/mcp/qmd/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "worker")
	w := httptest.NewRecorder()
	h, err := b.gatewayHandler("qmd")
	if err != nil {
		t.Fatal(err)
	}
	// Serve exactly as JailHandler does: prefix-stripped.
	http.StripPrefix("/mcp/qmd", h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if gotPath != "/mcp/" {
		t.Fatalf("upstream path = %q, want %q (backend path must compose with the stripped prefix)", gotPath, "/mcp/")
	}
}

func TestGatewayAuditCorrelatesTokenID(t *testing.T) {
	// The use-time allow line must carry the same token id the mint ledger
	// recorded, so a /request line can be tied to its later gateway use.
	var reached bool
	var gotBody string
	up := upstreamMCP(t, &reached, &gotBody)
	defer up.Close()
	cfg := testConfig(t)
	var buf bytes.Buffer
	cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	b := New(cfg)
	_ = b.reg.Register(regTool("db", up.URL, "read"))

	cap := mintFor(t, b, "worker", nil)
	raw, err := base64.RawURLEncoding.DecodeString(cap)
	if err != nil {
		t.Fatal(err)
	}
	id := token.ID(raw)
	if id == "" {
		t.Fatal("minted token must carry an id")
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"A","_capability":"` + cap + `"}}}`
	r := httptest.NewRequest("POST", "/mcp/db/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "worker")
	w := httptest.NewRecorder()
	h, err := b.gatewayHandler("db")
	if err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(buf.String(), "id="+id) {
		t.Fatalf("gateway allow audit must carry the token id %q, got: %s", id, buf.String())
	}
}

func TestGatewayVerifyDenyAuditCarriesClaimedID(t *testing.T) {
	// A verify failure (here: caller != bound_agent) should still log the
	// token's claimed id so the denied attempt correlates with its mint.
	var reached bool
	var gotBody string
	up := upstreamMCP(t, &reached, &gotBody)
	defer up.Close()
	cfg := testConfig(t)
	var buf bytes.Buffer
	cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	b := New(cfg)
	_ = b.reg.Register(regTool("db", up.URL, "read"))

	cap := mintFor(t, b, "worker", nil) // bound to worker
	raw, _ := base64.RawURLEncoding.DecodeString(cap)
	id := token.ID(raw)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"A","_capability":"` + cap + `"}}}`
	r := httptest.NewRequest("POST", "/mcp/db/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "analyst")
	w := httptest.NewRecorder()
	h, _ := b.gatewayHandler("db")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "decision=deny") || !strings.Contains(out, "id="+id) {
		t.Fatalf("verify-deny audit must carry the claimed token id %q, got: %s", id, out)
	}
}

func TestGatewayRevokedDenyAuditCarriesTokenID(t *testing.T) {
	// The post-revocation replay is exactly the denied use an operator greps
	// for after `lever revoke` — the deny line must still carry the token id.
	var reached bool
	var gotBody string
	up := upstreamMCP(t, &reached, &gotBody)
	defer up.Close()
	cfg := testConfig(t)
	var buf bytes.Buffer
	cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	b := New(cfg)
	_ = b.reg.Register(regTool("db", up.URL, "read"))

	cap := mintFor(t, b, "worker", nil)
	raw, _ := base64.RawURLEncoding.DecodeString(cap)
	id := token.ID(raw)
	b.Revoke("worker")
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"A","_capability":"` + cap + `"}}}`
	r := httptest.NewRequest("POST", "/mcp/db/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "worker")
	w := httptest.NewRecorder()
	h, _ := b.gatewayHandler("db")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (revoked)", w.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "detail=revoked") || !strings.Contains(out, "id="+id) {
		t.Fatalf("revoked deny must carry the token id %q, got: %s", id, out)
	}
}
