package broker

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The jail listener sets only ReadHeaderTimeout/IdleTimeout at the server
// level (ReadTimeout would kill /llm streaming). Per-route bounds close the
// slow-body/slow-handler hole: every non-streaming route runs under an
// http.TimeoutHandler, and every route — /llm included — puts a read
// deadline on the connection for the request body.

func withTimeouts(tc TimeoutConfig) configOpt {
	return func(c *Config) { c.Timeouts = tc }
}

// trickle returns a request body that delivers one byte, then blocks until
// the test ends (never EOF): the slow-loris body.
func trickle(t *testing.T) io.Reader {
	t.Helper()
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte("{")) }()
	t.Cleanup(func() { _ = pw.Close() })
	return pr
}

func TestTimeoutDefaults(t *testing.T) {
	b := New(testConfig(t))
	want := TimeoutConfig{Body: defaultJailBodyTimeout, Control: defaultJailControlTimeout,
		Tool: defaultJailToolTimeout, Worker: defaultJailWorkerTimeout}
	if b.timeouts != want {
		t.Fatalf("timeouts = %+v, want defaults %+v", b.timeouts, want)
	}
	b = New(testConfig(t, withTimeouts(TimeoutConfig{Control: time.Second})))
	if b.timeouts.Control != time.Second || b.timeouts.Body != defaultJailBodyTimeout {
		t.Fatalf("partial override must keep the other defaults: %+v", b.timeouts)
	}
}

// A control route whose body never arrives returns 503 at the control
// deadline instead of holding the handler for as long as the client likes.
func TestJailControlRouteBoundedOnTricklingBody(t *testing.T) {
	b := New(testConfig(t, withTimeouts(TimeoutConfig{Control: 200 * time.Millisecond})))
	h := b.JailHandler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/enrol", trickle(t)) // certless route: reads the body first
	start := time.Now()
	h.ServeHTTP(rec, req)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("handler held for %v; want a return near the 200ms deadline", el)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (timed out)", rec.Code)
	}
}

// A tool route is bounded by the tool deadline: a backend that never answers
// cannot pin the gateway.
func TestJailToolRouteBoundedOnSlowBackend(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	// LIFO cleanups: release the stuck backend handler BEFORE up.Close waits on it.
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(release) })
	b := New(testConfig(t, withTimeouts(TimeoutConfig{Tool: 200 * time.Millisecond, Control: time.Hour})))
	_ = b.reg.Register(regTool("slow", up.URL, "read"))
	h := b.JailHandler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp/slow/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	req.TLS = leafFor(t, b, "worker")
	start := time.Now()
	h.ServeHTTP(rec, req)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("tool route held for %v; want a return near the 200ms deadline", el)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (timed out)", rec.Code)
	}
}

// /llm is exempt from the handler deadline: a streamed completion outlives
// the control/tool bounds and is delivered whole.
func TestJailLLMRouteStreamsPastHandlerDeadlines(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: one\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(w, "data: two\n\n")
	}))
	defer up.Close()
	short := TimeoutConfig{Control: 100 * time.Millisecond, Tool: 100 * time.Millisecond, Worker: 100 * time.Millisecond}
	b := New(testConfig(t, withLLM(t, []byte("sk"), up.URL), withTimeouts(short)))
	tok := mintLLM(t, b.keys.Private, "worker", b.MinEpoch())

	rec := httptest.NewRecorder()
	req := newMTLSRequest(t, b, "worker", http.MethodPost, "/llm/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	b.JailHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (llm must not be handler-bounded)", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "data: one") || !strings.Contains(body, "data: two") {
		t.Fatalf("stream truncated: %q", body)
	}
}

// On a real connection, the request BODY read on /llm is bounded by the body
// deadline even though the handler itself is not: a client that opens a
// request and then trickles the body gets an answer (502: the upstream write
// failed on the read timeout) instead of holding a broker goroutine and an
// upstream connection open indefinitely.
func TestJailLLMBodyReadIsBoundedOnRealConnection(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body) // a real upstream reads the request before answering
		_, _ = io.WriteString(w, `{"type":"message"}`)
	}))
	defer up.Close()
	b := New(testConfig(t, withLLM(t, []byte("sk"), up.URL),
		withTimeouts(TimeoutConfig{Body: 300 * time.Millisecond})))
	tok := mintLLM(t, b.keys.Private, "worker", b.MinEpoch())
	srv := jailServer(t, b)
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(b.ca.Cert)
	conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), &tls.Config{
		RootCAs: pool, ServerName: e2eServerName,
		Certificates: []tls.Certificate{signedCert(t, b, "worker")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Headers promise 1000 bytes; deliver one and stall.
	fmt.Fprintf(conn, "POST /llm/v1/messages HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{", e2eServerName, tok)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response within 5s (body read is unbounded): %v", err)
	}
	defer resp.Body.Close()
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("response took %v; want near the 300ms body deadline", el)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (upstream write aborted by the body deadline)", resp.StatusCode)
	}
}
