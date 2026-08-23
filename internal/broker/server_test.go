package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	registry "github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/wire"
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
	// Pre-load the config envelope for "calendar" (config-authoritative: tool
	// cannot register unless the host config already knows about it).
	_ = b.reg.Register(registry.Tool{
		Name: "calendar", Backend: "http://127.0.0.1:3203",
		Operations: map[string]registry.Operation{"list": {Name: "list"}},
	})
	h := b.AdminHandler()

	body, _ := json.Marshal(wire.RegisterRequest{
		Name:       "calendar",
		Backend:    "http://127.0.0.1:3203",
		Operations: []wire.OperationSpec{{Name: "list"}},
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

	r := httptest.NewRequest("POST", "/provision", bytes.NewReader([]byte(`{"worker":"worker"}`)))
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

func TestServeListenersRejectsNonLoopbackAdmin(t *testing.T) {
	b := New(testConfig(t))
	jailLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Bind admin on a non-loopback-guaranteed wildcard address (0.0.0.0).
	adminLn, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	certSrc, err := b.ca.NewServerCertSource("host.orb.internal", []string{"host.orb.internal"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- b.ServeListeners(context.Background(), jailLn, adminLn, nil, certSrc) }()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("ServeListeners must reject a non-loopback admin listener")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeListeners did not fail closed on a non-loopback admin listener")
	}
}

// Every jail JSON route is POST-only and /tools is GET-only: a wrong method
// 405s at the mux before any handler (and so before any audit line) runs.
// The admin mux follows the same pattern.
func TestRoutesRejectWrongMethod(t *testing.T) {
	b := New(testConfig(t))
	jail := b.JailHandler()
	admin := b.AdminHandler()
	cases := []struct {
		h      http.Handler
		method string
		path   string
	}{
		{jail, http.MethodGet, wire.PathProvision},
		{jail, http.MethodGet, wire.PathWorkerStart},
		{jail, http.MethodGet, wire.PathWorkerList},
		{jail, http.MethodGet, wire.PathMsgSend},
		{jail, http.MethodGet, wire.PathMsgList},
		{jail, http.MethodGet, wire.PathDirectiveConsume},
		{jail, http.MethodGet, wire.PathDirectiveCheck},
		{jail, http.MethodGet, wire.PathEnrol},
		{jail, http.MethodGet, wire.PathRenew},
		{jail, http.MethodGet, wire.PathRequest},
		{jail, http.MethodPost, wire.PathTools},
		{admin, http.MethodGet, wire.PathRegister},
		{admin, http.MethodPost, wire.PathEpoch},
		{admin, http.MethodGet, wire.PathBumpEpoch},
		{admin, http.MethodGet, wire.PathRevoke},
		{admin, http.MethodGet, wire.PathBootstrap},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, tc.path, bytes.NewReader([]byte(`{}`)))
		r.TLS = leafFor(t, b, "manager")
		w := httptest.NewRecorder()
		tc.h.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: status = %d, want 405", tc.method, tc.path, w.Code)
		}
	}
}
