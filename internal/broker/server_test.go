package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestJailHandlerGatewayDeniesNoCert verifies that a tools/call request to the
// jail mux's /mcp/<tool>/ path with no client cert is rejected 403.
// The gateway is bound at JailHandler() time, so the tool must be registered
// BEFORE calling JailHandler().
func TestJailHandlerGatewayDeniesNoCert(t *testing.T) {
	var reached bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer up.Close()

	b := New(testConfig(t))
	// Register a tool BEFORE calling JailHandler() — gateways are bound at call time.
	if err := b.reg.Register(regTool("db", up.URL, "read")); err != nil {
		t.Fatal(err)
	}

	h := b.JailHandler()

	// POST with no client cert — RequireAgent must reject with 403.
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"A","_capability":"fake"}}}`
	r := httptest.NewRequest("POST", "/mcp/db/", bytes.NewReader([]byte(body)))
	// Deliberately no r.TLS set → no client cert.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no client cert must deny)", w.Code)
	}
	if reached {
		t.Fatal("SECURITY: backend reached despite missing client cert")
	}
}

// TestAdminHandlerRegisterAddsTool verifies that a POST to /register on the admin
// handler adds the tool to the registry and returns 200.
func TestAdminHandlerRegisterAddsTool(t *testing.T) {
	b := New(testConfig(t))
	h := b.AdminHandler()

	body, _ := json.Marshal(RegisterRequest{
		Name:       "calendar",
		Backend:    "http://127.0.0.1:3203",
		Operations: []OperationSpec{{Name: "list"}},
	})
	r := httptest.NewRequest("POST", "/register", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !b.reg.HasOperation("calendar", "list") {
		t.Fatal("calendar.list should be registered after POST /register")
	}
}

// TestJailHandlerRoutesProvision confirms the jail mux wires /provision correctly.
// We test without a TLS state to trigger an auth error — the handler must not 404.
func TestJailHandlerRoutesProvision(t *testing.T) {
	b := New(testConfig(t))
	h := b.JailHandler()

	r := httptest.NewRequest("POST", "/provision", bytes.NewReader([]byte(`{"grove":"worker"}`)))
	// No TLS → handleProvision will reject (not manager) but must NOT 404.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code == http.StatusNotFound {
		t.Fatalf("/provision returned 404 — not routed in JailHandler")
	}
}

// TestAdminHandlerDoesNotExposeProvision verifies that /provision is NOT served
// on the admin handler (should 404 or 405).
func TestAdminHandlerDoesNotExposeProvision(t *testing.T) {
	b := New(testConfig(t))
	h := b.AdminHandler()

	r := httptest.NewRequest("POST", "/provision", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("/provision on admin handler returned %d, want 404 (should not be exposed)", w.Code)
	}
}
