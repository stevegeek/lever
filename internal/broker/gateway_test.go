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

// gatewayRig is a broker with one tool registered at a recording upstream,
// plus what that upstream saw. Every gateway test posts through it.
type gatewayRig struct {
	t       *testing.T
	b       *Broker
	tool    string
	reached bool
	gotBody string
}

// newGatewayRig starts the recording upstream, builds a broker from cfg and
// registers reg(upstreamURL) as the rig's tool. reg is regTool/regCoarseTool
// style: it receives the upstream's URL as the backend.
func newGatewayRig(t *testing.T, cfg Config, reg func(backend string) registry.Tool) *gatewayRig {
	t.Helper()
	g := &gatewayRig{t: t}
	up := upstreamMCP(t, &g.reached, &g.gotBody)
	t.Cleanup(up.Close)
	g.b = New(cfg)
	tool := reg(up.URL)
	g.tool = tool.Name
	if err := g.b.reg.Register(tool); err != nil {
		t.Fatal(err)
	}
	return g
}

// dbRig is the default rig: testConfig and the fine-grained "db" tool with
// the "read" operation.
func dbRig(t *testing.T) *gatewayRig {
	t.Helper()
	return newGatewayRig(t, testConfig(t), func(u string) registry.Tool { return regTool("db", u, "read") })
}

// post sends body to the tool's root as the given caller CN through the
// tool's gateway handler.
func (g *gatewayRig) post(caller, body string) *httptest.ResponseRecorder {
	g.t.Helper()
	r := httptest.NewRequest("POST", "/mcp/"+g.tool+"/", strings.NewReader(body))
	r.TLS = leafFor(g.t, g.b, caller)
	w := httptest.NewRecorder()
	h, err := g.b.gatewayHandler(g.tool)
	if err != nil {
		g.t.Fatal(err)
	}
	h.ServeHTTP(w, r)
	return w
}

// assertForwarded pins the allow shape: 200, the backend reached, and the
// token stripped from what it received.
func (g *gatewayRig) assertForwarded(w *httptest.ResponseRecorder, why string) {
	g.t.Helper()
	if w.Code != http.StatusOK {
		g.t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !g.reached {
		g.t.Fatalf("backend should be reached: %s", why)
	}
	if bytes.Contains([]byte(g.gotBody), []byte("_capability")) {
		g.t.Fatalf("token leaked upstream: %s", g.gotBody)
	}
}

// assertDenied pins the deny shape: 403 and the backend never reached.
func (g *gatewayRig) assertDenied(w *httptest.ResponseRecorder, why string) {
	g.t.Helper()
	if w.Code != http.StatusForbidden {
		g.t.Fatalf("status = %d, want 403 (%s)", w.Code, why)
	}
	if g.reached {
		g.t.Fatalf("SECURITY: backend reached despite %s", why)
	}
}

// toolsCall builds a JSON-RPC tools/call body for name. args are the inner
// fields of the arguments object; cap, when non-empty, is appended as
// _capability.
func toolsCall(name, args, cap string) string {
	if cap != "" {
		if args != "" {
			args += ","
		}
		args += `"_capability":"` + cap + `"`
	}
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{` + args + `}}}`
}

func TestGatewayAllowsValidCapabilityAndStripsIt(t *testing.T) {
	g := dbRig(t)
	cap := mintFor(t, g.b, "worker", nil)
	g.assertForwarded(g.post("worker", toolsCall("read", `"table":"A"`, cap)), "a valid call")
}

func TestGatewayDeniesMissingCapabilityWithoutReachingBackend(t *testing.T) {
	g := dbRig(t)
	g.assertDenied(g.post("worker", toolsCall("read", `"table":"A"`, "")), "missing capability")
}

// pathRecordingUpstream records the exact URL.Path it was reached at.
func pathRecordingUpstream(t *testing.T, gotPath *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
}

// The common case — a path-less backend — is untouched: the tool root still
// forwards as "/". (Path-suffixed backends are covered by
// TestGatewayComposesBackendPath.)
func TestGatewayPathlessBackendRootUnchanged(t *testing.T) {
	var gotPath string
	up := pathRecordingUpstream(t, &gotPath)
	defer up.Close()
	b := New(testConfig(t))
	_ = b.reg.Register(regTool("db", up.URL, "read"))

	h, err := b.gatewayHandler("db")
	if err != nil {
		t.Fatal(err)
	}
	handler := http.StripPrefix("/mcp/db", h)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	r := httptest.NewRequest("POST", "/mcp/db/", bytes.NewReader([]byte(body)))
	r.TLS = leafFor(t, b, "worker")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if gotPath != "/" {
		t.Fatalf("path-less backend path = %q, want %q (unchanged)", gotPath, "/")
	}
}

func TestGatewayDeniesWrongCallerWithoutReachingBackend(t *testing.T) {
	g := dbRig(t)
	cap := mintFor(t, g.b, "worker", nil) // bound to worker
	// caller is analyst, not worker
	g.assertDenied(g.post("analyst", toolsCall("read", `"table":"A"`, cap)), "caller != bound_agent")
}

func TestGatewayEnforcesConstraintAgainstArgs(t *testing.T) {
	g := dbRig(t)
	cap := mintFor(t, g.b, "worker", map[string]string{"table": "A"}) // constrained to table A
	// Request asks for table C -> must be denied.
	g.assertDenied(g.post("worker", toolsCall("read", `"table":"C"`, cap)), "constraint table==A, request table==C")
}

// --- Regression tests for FIX 1 (gateway level): object arg must NOT satisfy empty-string constraint ---

// TestGatewayDeniesObjectArgAgainstEmptyStringConstraint is the end-to-end bypass
// regression. A token constrained to table="" must NOT be satisfied by table={"x":1}
// (or any non-string). Before the fix, mcp.ToolsCall coerced {"x":1} to "" and the
// token.Verify passed — backend reached. After the fix, {"x":1} projects to `{"x":1}`
// which does not equal "", so token.Verify denies — backend NOT reached (403).
func TestGatewayDeniesObjectArgAgainstEmptyStringConstraint(t *testing.T) {
	g := dbRig(t)
	// Token constrained to table="" — should only allow a literal empty string.
	cap := mintFor(t, g.b, "worker", map[string]string{"table": ""})
	// Request sends table as a JSON object (injection attempt).
	g.assertDenied(g.post("worker", toolsCall("read", `"table":{"x":1}`, cap)),
		"object arg bypassed empty-string constraint (token.Verify)")
}

// --- Regression tests for FIX 2: method allowlist (fail-closed) ---

// TestGatewayDeniesUnknownMethodWithoutReachingBackend verifies that a JSON-RPC
// method outside the explicit allowlist is rejected 403 and the backend is NOT
// reached. Before the fix, the default: branch forwarded everything — fail-open.
func TestGatewayDeniesUnknownMethodWithoutReachingBackend(t *testing.T) {
	g := dbRig(t)
	g.assertDenied(g.post("worker", `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{}}`),
		"unknown method (fail-open bypass)")
}

// TestGatewayForwardsInitialize verifies that the allowlisted `initialize` method
// is forwarded to the backend unchanged (no capability required).
func TestGatewayForwardsInitialize(t *testing.T) {
	g := dbRig(t)
	w := g.post("worker", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}}}`)
	g.assertForwarded(w, "initialize must be forwarded")
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

// things3Rig is the coarse-tool rig: an external tool with only the wildcard op.
func things3Rig(t *testing.T) *gatewayRig {
	t.Helper()
	return newGatewayRig(t, testConfig(t), func(u string) registry.Tool { return regCoarseTool("things3", u) })
}

func TestGatewayCoarseToolAcceptsWildcardCapability(t *testing.T) {
	g := things3Rig(t)
	cap := mintOp(t, g.b, "worker", "things3", registry.WildcardOp)
	g.assertForwarded(g.post("worker", toolsCall("add-todo", `"title":"x"`, cap)), "wildcard capability on a coarse tool")
}

func TestGatewayCoarseToolDeniesPerOpCapability(t *testing.T) {
	g := things3Rig(t)
	// A token naming the specific MCP tool must NOT satisfy a coarse tool: the
	// gateway requires exactly {things3, "*"} there.
	cap := mintOp(t, g.b, "worker", "things3", "add-todo")
	g.assertDenied(g.post("worker", toolsCall("add-todo", "", cap)), "a non-wildcard token on a coarse tool")
}

func TestGatewayFineToolDeniesWildcardCapability(t *testing.T) {
	g := dbRig(t) // fine (Coarse=false)
	// A wildcard token must NOT satisfy a fine tool: the gateway requires the
	// real params.name there, and "db" has no "*" operation.
	cap := mintOp(t, g.b, "worker", "db", registry.WildcardOp)
	g.assertDenied(g.post("worker", toolsCall("read", `"table":"A"`, cap)), "a wildcard token on a fine tool")
}

func TestGatewayCoarseCapabilityIsIdentityBound(t *testing.T) {
	g := things3Rig(t)
	cap := mintOp(t, g.b, "worker", "things3", registry.WildcardOp) // bound to worker
	// replay by a different agent
	g.assertDenied(g.post("analyst", toolsCall("add-todo", "", cap)), "a cross-agent coarse replay")
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
	// Hitting the tool root must forward to the backend's path EXACTLY ("/mcp"),
	// not "/mcp/": the trailing slash is an artifact of the broker's subtree mux,
	// and a strict streamable-HTTP endpoint (qmd) 404s on it (verified live).
	if gotPath != "/mcp" {
		t.Fatalf("upstream path = %q, want %q (trailing slash is a mux artifact; strict MCP endpoints 404 on it)", gotPath, "/mcp")
	}
}

// loggedDBRig is dbRig with the broker's log captured, plus a worker-bound
// db.read token and its id, for the audit-correlation tests.
func loggedDBRig(t *testing.T) (g *gatewayRig, log *bytes.Buffer, cap, id string) {
	t.Helper()
	cfg := testConfig(t)
	var buf bytes.Buffer
	cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	g = newGatewayRig(t, cfg, func(u string) registry.Tool { return regTool("db", u, "read") })
	cap = mintFor(t, g.b, "worker", nil)
	raw, err := base64.RawURLEncoding.DecodeString(cap)
	if err != nil {
		t.Fatal(err)
	}
	id = token.ID(raw)
	if id == "" {
		t.Fatal("minted token must carry an id")
	}
	return g, &buf, cap, id
}

func TestGatewayAuditCorrelatesTokenID(t *testing.T) {
	// The use-time allow line must carry the same token id the mint ledger
	// recorded, so a /request line can be tied to its later gateway use.
	g, buf, cap, id := loggedDBRig(t)
	w := g.post("worker", toolsCall("read", `"table":"A"`, cap))
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
	g, buf, cap, id := loggedDBRig(t) // token bound to worker
	w := g.post("analyst", toolsCall("read", `"table":"A"`, cap))
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
	g, buf, cap, id := loggedDBRig(t)
	g.b.Revoke("worker")
	w := g.post("worker", toolsCall("read", `"table":"A"`, cap))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (revoked)", w.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "detail=revoked") || !strings.Contains(out, "id="+id) {
		t.Fatalf("revoked deny must carry the token id %q, got: %s", id, out)
	}
}

// An MCP body over gatewayBodyLimit is rejected with 400 and an audited
// "bad body" deny before any parse or authz — the backend is never reached.
func TestGatewayRejectsOversizedBody(t *testing.T) {
	cfg, audit := auditConfig(t)
	g := newGatewayRig(t, cfg, func(u string) registry.Tool { return regTool("db", u, "read") })

	body := `{"jsonrpc":"2.0","id":1,"method":"ping","pad":"` + strings.Repeat("x", gatewayBodyLimit) + `"}`
	w := g.post("worker", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if g.reached {
		t.Fatal("backend reached despite oversized body")
	}
	if !strings.Contains(audit.String(), `detail="bad body"`) {
		t.Fatalf("audit missing bad body deny: %s", audit.String())
	}
}

func TestBackendURL(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:3201":        "http://127.0.0.1:3201",
		"[::1]:3101/mcp":        "http://[::1]:3101/mcp",
		"https://h.example/mcp": "https://h.example/mcp",
	}
	for in, want := range cases {
		u, err := backendURL(in)
		if err != nil || u.String() != want {
			t.Fatalf("backendURL(%q) = %v, %v; want %q", in, u, err, want)
		}
	}
}
