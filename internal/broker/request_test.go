package broker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lever-to/lever/internal/cap/token"
)

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
