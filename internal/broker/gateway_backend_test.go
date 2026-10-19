package broker

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/broker/registry"
)

// downBackend returns the URL of a backend that refuses connections.
func downBackend(t *testing.T) string {
	t.Helper()
	up := httptest.NewServer(http.NotFoundHandler())
	u := up.URL
	up.Close()
	return u
}

func TestGatewayBackendDownAnswers503AndAuditsOnce(t *testing.T) {
	backend := downBackend(t)
	cfg, audit := auditConfig(t)
	b := New(cfg)
	if err := b.reg.Register(regTool("db", backend, "read")); err != nil {
		t.Fatal(err)
	}
	g := &gatewayRig{t: t, b: b, tool: "db"}
	cap := mintFor(t, b, "worker", nil)

	for i := 0; i < 3; i++ {
		w := g.post("worker", toolsCall("read", `"table":"A"`, cap))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("call %d: status = %d, want 503; body = %s", i, w.Code, w.Body)
		}
		if got, want := w.Body.String(), "tool db: backend unavailable; try again later\n"; got != want {
			t.Fatalf("body = %q, want %q", got, want)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("content type = %q, want text/plain", ct)
		}
	}
	host := mustHost(t, backend)
	port := host[strings.LastIndex(host, ":")+1:]
	out := audit.String()
	if n := strings.Count(out, "gateway.backend_unavailable"); n != 1 {
		t.Fatalf("backend_unavailable lines = %d, want 1 (rate-limited):\n%s", n, out)
	}
	if !strings.Contains(out, "decision=error") || !strings.Contains(out, "backend="+host) ||
		!strings.Contains(out, "caller=worker") || !strings.Contains(out, "op=db") {
		t.Fatalf("audit line lacks tool, caller or backend:\n%s", out)
	}
	// The agent never sees the backend address.
	w := g.post("worker", toolsCall("read", `"table":"A"`, cap))
	if strings.Contains(w.Body.String(), port) || strings.Contains(w.Body.String(), "127.0.0.1") {
		t.Fatalf("SECURITY: backend address leaked to the agent: %q", w.Body)
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func TestBackendErrLimit(t *testing.T) {
	l := &backendErrLimit{every: time.Minute}
	t0 := time.Unix(1000, 0)
	if ok, n := l.allow(t0); !ok || n != 0 {
		t.Fatalf("first = %v %d, want true 0", ok, n)
	}
	for i := 1; i <= 3; i++ {
		if ok, _ := l.allow(t0.Add(time.Duration(i) * time.Second)); ok {
			t.Fatalf("failure %d inside the interval was logged", i)
		}
	}
	if ok, n := l.allow(t0.Add(time.Minute)); !ok || n != 3 {
		t.Fatalf("after interval = %v %d, want true 3", ok, n)
	}
}

// A backend that drops the connection gives 503; once back, its own 404 for
// an unknown session passes through unchanged so the client re-initializes,
// and an allowed call works again.
func TestGatewayBackendRecoveryPassesSession404Through(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		if r.Header.Get("Mcp-Session-Id") == "stale" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Backend", "own")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"Session not found"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}))
	t.Cleanup(up.Close)
	b := New(testConfig(t))
	if err := b.reg.Register(regTool("db", up.URL, "read")); err != nil {
		t.Fatal(err)
	}
	h, err := b.gatewayHandler("db")
	if err != nil {
		t.Fatal(err)
	}
	cap := mintFor(t, b, "worker", nil)
	send := func(session string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/mcp/db/", strings.NewReader(toolsCall("read", `"table":"A"`, cap)))
		r.TLS = leafFor(t, b, "worker")
		if session != "" {
			r.Header.Set("Mcp-Session-Id", session)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	if w := send("stale"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("down: status = %d, want 503", w.Code)
	}
	down.Store(false)
	w := send("stale")
	if w.Code != http.StatusNotFound || w.Header().Get("X-Backend") != "own" ||
		!strings.Contains(w.Body.String(), "Session not found") {
		t.Fatalf("session 404 not passed through: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if w := send(""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("allowed call after recovery: %d %s", w.Code, w.Body)
	}
}

func TestGatewayNonPostIs405(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut, http.MethodPatch} {
		g := dbRig(t)
		r := httptest.NewRequest(method, "/mcp/db/", strings.NewReader(toolsCall("read", `"table":"A"`, mintFor(t, g.b, "worker", nil))))
		r.TLS = leafFor(t, g.b, "worker")
		r.Header.Set("Accept", "text/event-stream")
		w := httptest.NewRecorder()
		h, err := g.b.gatewayHandler("db")
		if err != nil {
			t.Fatal(err)
		}
		h.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("%s: %d Allow=%q, want 405 Allow=POST", method, w.Code, w.Header().Get("Allow"))
		}
		if g.reached {
			t.Fatalf("SECURITY: %s reached the backend", method)
		}
	}
}

// Identity is checked before the method: a caller without a client cert
// still gets 403, not 405.
func TestGatewayNonPostWithoutIdentityIsForbidden(t *testing.T) {
	g := dbRig(t)
	r := httptest.NewRequest(http.MethodGet, "/mcp/db/", nil)
	w := httptest.NewRecorder()
	h, _ := g.b.gatewayHandler("db")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestGatewayAuditsRevokedNonPost(t *testing.T) {
	cfg, audit := auditConfig(t)
	g := newGatewayRig(t, cfg, func(u string) registry.Tool { return regTool("db", u, "read") })
	g.b.Revoke("worker")
	audit.Reset()
	r := httptest.NewRequest(http.MethodGet, "/mcp/db/", nil)
	r.TLS = leafFor(t, g.b, "worker")
	w := httptest.NewRecorder()
	h, _ := g.b.gatewayHandler("db")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	if !strings.Contains(audit.String(), "decision=deny") || !strings.Contains(audit.String(), "revoked: GET") {
		t.Fatalf("no audit deny for a revoked caller: %s", audit)
	}
}

// Gating runs before any proxying: with the backend down, a denied call is
// still a 403 and writes no backend failure.
func TestGatewayDeniesBeforeProxyWhenBackendDown(t *testing.T) {
	cfg, audit := auditConfig(t)
	b := New(cfg)
	if err := b.reg.Register(regTool("db", downBackend(t), "read")); err != nil {
		t.Fatal(err)
	}
	g := &gatewayRig{t: t, b: b, tool: "db"}
	cases := map[string]string{
		"missing capability": toolsCall("read", `"table":"A"`, ""),
		"wrong caller":       toolsCall("read", `"table":"A"`, mintFor(t, b, "analyst", nil)),
		"not allowlisted":    `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{}}`,
	}
	for why, body := range cases {
		if w := g.post("worker", body); w.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403", why, w.Code)
		}
	}
	if strings.Contains(audit.String(), "backend_unavailable") {
		t.Fatalf("SECURITY: a denied call was proxied:\n%s", audit)
	}
}
