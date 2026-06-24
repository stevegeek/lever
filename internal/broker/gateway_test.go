package broker

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lever-to/lever/internal/cap/token"
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
