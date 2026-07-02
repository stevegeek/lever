package broker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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
