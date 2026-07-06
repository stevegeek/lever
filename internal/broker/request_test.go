package broker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lever-to/lever/internal/broker/registry"
	"github.com/lever-to/lever/internal/broker/rules"
	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
)

// restrictedConfig builds a broker whose "db" tool restricts the "table"
// constraint to {A,B} via AllowedValues, and lets analyst self-obtain db.read.
// Used to exercise the broker's mint-time constraint validation + baking.
func restrictedConfig(t *testing.T) Config {
	t.Helper()
	kp, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	rl := rules.NewPolicy()
	rl.AllowObtain("analyst", "db", "read")
	reg := registry.New()
	if err := reg.Register(registry.Tool{
		Name: "db", Backend: "http://127.0.0.1:3201",
		Operations:    map[string]registry.Operation{"read": {Name: "read"}},
		AllowedValues: map[string][]string{"table": {"A", "B"}},
	}); err != nil {
		t.Fatal(err)
	}
	return Config{
		Keys: kp, CA: c, Tickets: ca.NewTicketStore(), Rules: rl, Registry: reg,
		ManagerIdentity: "manager", Agents: []string{"manager", "analyst"},
		GrantTTL: time.Hour, ServerName: "host.orb.internal",
	}
}

func TestRequestConstraintMintsNarrowedToken(t *testing.T) {
	// The constraint loop (request.go:63-66) must bake every requested constraint
	// into the minted token so it can ONLY be used for that exact param value.
	b := New(restrictedConfig(t))
	r := httptest.NewRequest("POST", "/request", reqBody(t, CapRequest{
		Tool: "db", Op: "read", BoundTo: "analyst", Constraints: map[string]string{"table": "A"},
	}))
	r.TLS = leafFor(t, b, "analyst")
	w := httptest.NewRecorder()
	b.handleRequest(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp CapResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	raw, _ := base64.RawURLEncoding.DecodeString(resp.Token)
	// Verifies only when the request carries table=A...
	if err := token.Verify(b.keys.Public, raw, token.Request{
		Caller: "analyst", Capability: token.Capability{Tool: "db", Operation: "read"},
		Params: map[string]string{"table": "A"}, Now: time.Now(), MinEpoch: 0,
	}); err != nil {
		t.Fatalf("constrained token must verify with table=A: %v", err)
	}
	// ...and is denied when the constrained param is absent.
	if err := token.Verify(b.keys.Public, raw, token.Request{
		Caller: "analyst", Capability: token.Capability{Tool: "db", Operation: "read"},
		Params: map[string]string{}, Now: time.Now(), MinEpoch: 0,
	}); err == nil {
		t.Fatal("constrained token must NOT verify without the baked-in table constraint")
	}
}

func TestRequestDeniesConstraintValueOutsideAllowedSet(t *testing.T) {
	// ValidateConstraints (request.go:58) must fail closed when a requested value is
	// outside the tool's AllowedValues — no token is minted.
	b := New(restrictedConfig(t))
	r := httptest.NewRequest("POST", "/request", reqBody(t, CapRequest{
		Tool: "db", Op: "read", BoundTo: "analyst", Constraints: map[string]string{"table": "secrets"},
	}))
	r.TLS = leafFor(t, b, "analyst")
	w := httptest.NewRecorder()
	b.handleRequest(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (table=secrets is outside AllowedValues {A,B})", w.Code)
	}
}

func reqBody(t *testing.T, cr CapRequest) *bytes.Reader {
	t.Helper()
	body, _ := json.Marshal(cr)
	return bytes.NewReader(body)
}

func TestRequestSelfObtainMintsUsableToken(t *testing.T) {
	b := New(testConfig(t))
	r := httptest.NewRequest("POST", "/request", reqBody(t, CapRequest{Tool: "db", Op: "read", BoundTo: "analyst"}))
	r.TLS = leafFor(t, b, "analyst") // analyst self-obtains db.read
	w := httptest.NewRecorder()
	b.handleRequest(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp CapResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	raw, _ := base64.RawURLEncoding.DecodeString(resp.Token)
	if err := token.Verify(b.keys.Public, raw, token.Request{
		Caller: "analyst", Capability: token.Capability{Tool: "db", Operation: "read"},
		Params: map[string]string{}, Now: time.Now(), MinEpoch: 0,
	}); err != nil {
		t.Fatalf("minted token failed to verify for analyst: %v", err)
	}
}

func TestRequestDelegationMintsTokenBoundToRecipient(t *testing.T) {
	b := New(testConfig(t))
	r := httptest.NewRequest("POST", "/request", reqBody(t, CapRequest{Tool: "db", Op: "read", BoundTo: "worker"}))
	r.TLS = leafFor(t, b, "manager") // manager delegates db.read to worker
	w := httptest.NewRecorder()
	b.handleRequest(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp CapResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	raw, _ := base64.RawURLEncoding.DecodeString(resp.Token)
	// Bound to worker: verifies for worker, NOT for manager.
	if err := token.Verify(b.keys.Public, raw, token.Request{
		Caller: "worker", Capability: token.Capability{Tool: "db", Operation: "read"},
		Params: map[string]string{}, Now: time.Now(), MinEpoch: 0,
	}); err != nil {
		t.Fatalf("token should verify for worker: %v", err)
	}
	if err := token.Verify(b.keys.Public, raw, token.Request{
		Caller: "manager", Capability: token.Capability{Tool: "db", Operation: "read"},
		Params: map[string]string{}, Now: time.Now(), MinEpoch: 0,
	}); err == nil {
		t.Fatal("delegated token must NOT verify for the delegator (manager)")
	}
}

func TestRequestDeniesUngrantedDelegation(t *testing.T) {
	b := New(testConfig(t))
	// analyst may self-obtain db.read but has no delegate grant.
	r := httptest.NewRequest("POST", "/request", reqBody(t, CapRequest{Tool: "db", Op: "read", BoundTo: "worker"}))
	r.TLS = leafFor(t, b, "analyst")
	w := httptest.NewRecorder()
	b.handleRequest(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (analyst cannot delegate)", w.Code)
	}
}

func TestRequestDeniesUnregisteredOperation(t *testing.T) {
	b := New(testConfig(t))
	r := httptest.NewRequest("POST", "/request", reqBody(t, CapRequest{Tool: "db", Op: "drop", BoundTo: "analyst"}))
	r.TLS = leafFor(t, b, "analyst")
	w := httptest.NewRecorder()
	b.handleRequest(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (db.drop not a registered op)", w.Code)
	}
}

// coarseConfig builds a broker with a single coarse, externally-fronted tool
// "utilities" (registry has ONLY the synthetic WildcardOp, mirroring a real
// gate:coarse tool). grantWildcard controls whether "analyst" holds the
// {utilities, "*"} obtain grant. The returned buffer captures audit log
// output so tests can assert on the op-coercion detail.
func coarseConfig(t *testing.T, grantWildcard bool) (Config, *bytes.Buffer) {
	t.Helper()
	kp, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	rl := rules.NewPolicy()
	if grantWildcard {
		rl.AllowObtain("analyst", "utilities", registry.WildcardOp)
	}
	reg := registry.New()
	if err := reg.Register(registry.Tool{
		Name: "utilities", Backend: "127.0.0.1:3103", External: true, Coarse: true,
		Operations: map[string]registry.Operation{registry.WildcardOp: {Name: registry.WildcardOp}},
	}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	return Config{
		Keys: kp, CA: c, Tickets: ca.NewTicketStore(), Rules: rl, Registry: reg,
		ManagerIdentity: "manager", Agents: []string{"manager", "analyst"},
		GrantTTL: time.Hour, ServerName: "host.orb.internal",
		Log: slog.New(slog.NewTextHandler(&buf, nil)),
	}, &buf
}

func TestRequestCoarseToolCoercesFineShapedOpToWildcard(t *testing.T) {
	// A dispatched agent cannot know a tool's gate, so a fine-shaped request
	// (op "get_weather") against a coarse tool ("utilities", registry exposes
	// only WildcardOp) must be coerced to "*" before the policy check, and the
	// minted token must carry "*" (what the gateway's coarse path verifies).
	cfg, audit := coarseConfig(t, true)
	b := New(cfg)
	r := httptest.NewRequest("POST", "/request", reqBody(t, CapRequest{
		Tool: "utilities", Op: "get_weather", BoundTo: "analyst",
	}))
	r.TLS = leafFor(t, b, "analyst")
	w := httptest.NewRecorder()
	b.handleRequest(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 (coarse tool + wildcard grant must mint)", w.Code, w.Body.String())
	}
	var resp CapResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	raw, _ := base64.RawURLEncoding.DecodeString(resp.Token)
	// The minted token's op is "*", not "get_weather".
	if err := token.Verify(b.keys.Public, raw, token.Request{
		Caller: "analyst", Capability: token.Capability{Tool: "utilities", Operation: registry.WildcardOp},
		Params: map[string]string{}, Now: time.Now(), MinEpoch: 0,
	}); err != nil {
		t.Fatalf("minted token must verify against the wildcard op: %v", err)
	}
	if err := token.Verify(b.keys.Public, raw, token.Request{
		Caller: "analyst", Capability: token.Capability{Tool: "utilities", Operation: "get_weather"},
		Params: map[string]string{}, Now: time.Now(), MinEpoch: 0,
	}); err == nil {
		t.Fatal("minted token must NOT verify against the originally requested fine op (op must be coerced, not preserved)")
	}
	if !strings.Contains(audit.String(), "get_weather -> *") {
		t.Fatalf("audit log must record the op coercion, got: %s", audit.String())
	}
}

func TestRequestCoarseToolDeniesFineShapedRequestWithoutWildcardGrant(t *testing.T) {
	// Coercion must not widen the policy check: a caller without the exact
	// {tool, "*"} grant is still denied, even though the tool is coarse.
	cfg, _ := coarseConfig(t, false)
	b := New(cfg)
	r := httptest.NewRequest("POST", "/request", reqBody(t, CapRequest{
		Tool: "utilities", Op: "get_weather", BoundTo: "analyst",
	}))
	r.TLS = leafFor(t, b, "analyst")
	w := httptest.NewRecorder()
	b.handleRequest(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no wildcard grant; coercion must not widen access)", w.Code)
	}
}

func TestRequestFineToolExactGrantExactOpStillMints(t *testing.T) {
	// Fine tools are untouched by the coercion: an exact grant + exact op
	// request still mints normally.
	b := New(testConfig(t))
	r := httptest.NewRequest("POST", "/request", reqBody(t, CapRequest{Tool: "db", Op: "read", BoundTo: "analyst"}))
	r.TLS = leafFor(t, b, "analyst")
	w := httptest.NewRecorder()
	b.handleRequest(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 (fine tool, exact grant, exact op)", w.Code, w.Body.String())
	}
}

func TestRequestDeniesWildcardMintOnFineTool(t *testing.T) {
	cfg := testConfig(t)
	// Simulate a policy misconfiguration that slipped past config validation:
	// the registry gate (HasOperation) must still deny — a fine tool has no "*".
	cfg.Rules.AllowObtain("analyst", "db", registry.WildcardOp)
	b := New(cfg)
	body := `{"tool":"db","op":"*"}`
	r := httptest.NewRequest("POST", "/request", strings.NewReader(body))
	r.TLS = leafFor(t, b, "analyst")
	w := httptest.NewRecorder()
	b.handleRequest(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (fine tool exposes no wildcard op)", w.Code)
	}
}

func TestRequestPolicyDenyAuditIncludesToolAndOp(t *testing.T) {
	// The /request deny audit must include the requested tool, the original op
	// (pre-coercion), and bound_to context so live denies can be debugged without
	// tmux pane forensics.
	cfg, audit := coarseConfig(t, false) // analyst has no wildcard grant
	b := New(cfg)
	r := httptest.NewRequest("POST", "/request", reqBody(t, CapRequest{
		Tool: "utilities", Op: "get_weather", BoundTo: "analyst",
	}))
	r.TLS = leafFor(t, b, "analyst")
	w := httptest.NewRecorder()
	b.handleRequest(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no wildcard grant)", w.Code)
	}
	auditStr := audit.String()
	// The audit detail must include the tool name.
	if !strings.Contains(auditStr, "tool=utilities") {
		t.Fatalf("policy deny audit must include tool name, got: %s", auditStr)
	}
	// The audit detail must include the ORIGINAL op (pre-coercion).
	if !strings.Contains(auditStr, "op=get_weather") {
		t.Fatalf("policy deny audit must include original op, got: %s", auditStr)
	}
}

func TestRequestDeniesRevokedCallerMintingAndDelegation(t *testing.T) {
	b := New(testConfig(t))
	b.Revoke("manager")
	// A revoked manager may not self-obtain...
	r := httptest.NewRequest("POST", "/request", reqBody(t, CapRequest{Tool: "db", Op: "read", BoundTo: "manager"}))
	r.TLS = leafFor(t, b, "manager")
	w := httptest.NewRecorder()
	b.handleRequest(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("revoked self-obtain: status = %d, want 403", w.Code)
	}
	// ...nor delegate a token bound to a still-valid agent (the channel this closes).
	r2 := httptest.NewRequest("POST", "/request", reqBody(t, CapRequest{Tool: "db", Op: "read", BoundTo: "worker"}))
	r2.TLS = leafFor(t, b, "manager")
	w2 := httptest.NewRecorder()
	b.handleRequest(w2, r2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("revoked delegation: status = %d, want 403", w2.Code)
	}
}

func TestRequestAllowAuditCarriesMintLedger(t *testing.T) {
	// The allow line is the mint ledger: it must carry the token id (for
	// correlation with later gateway/llm use), the matched policy rule (the
	// "why"), and the minted claims (expiry, epoch, baked constraints).
	cfg := restrictedConfig(t)
	var buf bytes.Buffer
	cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	b := New(cfg)
	r := httptest.NewRequest("POST", "/request", reqBody(t, CapRequest{
		Tool: "db", Op: "read", BoundTo: "analyst", Constraints: map[string]string{"table": "A"},
	}))
	r.TLS = leafFor(t, b, "analyst")
	w := httptest.NewRecorder()
	b.handleRequest(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp CapResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	raw, _ := base64.RawURLEncoding.DecodeString(resp.Token)
	id := token.ID(raw)
	if id == "" {
		t.Fatal("minted token must carry a token id")
	}
	audit := buf.String()
	for _, want := range []string{
		"id=" + id,
		"rule=obtain:analyst:db.read",
		"epoch=0",
		`constraints="table=A"`,
		"exp=",
	} {
		if !strings.Contains(audit, want) {
			t.Fatalf("mint allow audit missing %q, got: %s", want, audit)
		}
	}
}

func TestRequestDelegationAuditNamesDelegateRule(t *testing.T) {
	cfg := restrictedConfig(t)
	cfg.Rules.AllowDelegate("manager", "db", "read", "analyst")
	var buf bytes.Buffer
	cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	b := New(cfg)
	r := httptest.NewRequest("POST", "/request", reqBody(t, CapRequest{
		Tool: "db", Op: "read", BoundTo: "analyst",
	}))
	r.TLS = leafFor(t, b, "manager")
	w := httptest.NewRecorder()
	b.handleRequest(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(buf.String(), "rule=delegate:manager->analyst:db.read") {
		t.Fatalf("delegation allow audit must name the delegate rule, got: %s", buf.String())
	}
}
