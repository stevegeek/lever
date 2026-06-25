package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	registry "github.com/lever-to/lever/internal/broker/registry"
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

// TestResolveAdminAddr verifies the loopback-enforcement helper directly.
// These tests do not start any server or listener.
func TestResolveAdminAddr(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "empty host defaults to 127.0.0.1",
			input: ":9001",
			want:  "127.0.0.1:9001",
		},
		{
			name:  "explicit 127.0.0.1 accepted",
			input: "127.0.0.1:9001",
			want:  "127.0.0.1:9001",
		},
		{
			name:  "IPv6 loopback accepted",
			input: "[::1]:9001",
			want:  "[::1]:9001",
		},
		{
			name:    "0.0.0.0 rejected",
			input:   "0.0.0.0:9001",
			wantErr: true,
		},
		{
			name:    "routable private IP rejected",
			input:   "192.168.1.5:9001",
			wantErr: true,
		},
		{
			name:    "localhost hostname rejected (not a parseable loopback IP)",
			input:   "localhost:9001",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveAdminAddr(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveAdminAddr(%q) = %q, nil; want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAdminAddr(%q) unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("resolveAdminAddr(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestEpochEndpointReportsCurrentEpoch(t *testing.T) {
	b := New(testConfig(t))
	b.BumpEpoch()
	b.BumpEpoch() // epoch 2
	r := httptest.NewRequest("GET", "/epoch", nil)
	w := httptest.NewRecorder()
	b.AdminHandler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp EpochResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Epoch != 2 {
		t.Fatalf("epoch = %d, want 2", resp.Epoch)
	}
}
