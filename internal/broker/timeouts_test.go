package broker

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/scion"
)

// The jail listener sets only ReadHeaderTimeout/IdleTimeout at the server
// level (ReadTimeout would kill /llm streaming). Per-route bounds close the
// slow-body hole: every route — /llm, /mcp/<tool>/ and /worker/* included —
// puts a read deadline on the connection for the request body, and the small
// JSON control routes additionally run under an http.TimeoutHandler. Tool
// and worker routes are NOT handler-bounded: TimeoutHandler buffers the whole
// response, which would turn a streaming MCP backend into one delayed write
// and discard a long call's output at the bound (see JailHandler).

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

// dialJail opens a raw mTLS connection to srv as agent cn, with a 5 s read
// deadline so an assertion on a stalled response fails instead of hanging.
func dialJail(t *testing.T, b *Broker, srv *httptest.Server, cn string) *tls.Conn {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(b.ca.Cert)
	conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), &tls.Config{
		RootCAs: pool, ServerName: e2eServerName,
		Certificates: []tls.Certificate{signedCert(t, b, cn)},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	return conn
}

// readLineContaining consumes lines from br until one contains want; a read
// error first (the connection deadline included) fails the test with why.
func readLineContaining(t *testing.T, br *bufio.Reader, want, why string) {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if strings.Contains(line, want) {
			return
		}
		if err != nil {
			t.Fatalf("%s: %q not delivered: %v", why, want, err)
		}
	}
}

const initializeMsg = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`

func TestTimeoutDefaults(t *testing.T) {
	b := New(testConfig(t))
	want := TimeoutConfig{Body: defaultJailBodyTimeout, Control: defaultJailControlTimeout}
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

// A tool route's request-body read is bounded by the body deadline on a real
// connection: a client that opens a call and then trickles the body gets 400
// (the gateway's body read failed) near the deadline instead of holding a
// broker goroutine for as long as it likes. This is the only bound a tool
// route carries — the backend's response time is deliberately not one.
func TestJailToolRouteBodyReadIsBounded(t *testing.T) {
	var reached bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer up.Close()
	b := New(testConfig(t, withTimeouts(TimeoutConfig{Body: 300 * time.Millisecond})))
	_ = b.reg.Register(regTool("slow", up.URL, "read"))
	srv := jailServer(t, b)
	defer srv.Close()

	conn := dialJail(t, b, srv, "worker")
	// Headers promise 1000 bytes; deliver one and stall.
	fmt.Fprintf(conn, "POST /mcp/slow/ HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{", e2eServerName)
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response within 5s (body read is unbounded): %v", err)
	}
	defer resp.Body.Close()
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("response took %v; want near the 300ms body deadline", el)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (gateway body read cut by the deadline)", resp.StatusCode)
	}
	if reached {
		t.Fatal("a request whose body never arrived must not reach the backend")
	}
}

// A streaming backend behind /mcp/<tool>/ (MCP streamable-HTTP answers with
// text/event-stream) is delivered incrementally: the client sees the first
// event BEFORE the backend has finished. A buffering wrapper (an
// http.TimeoutHandler) would hold everything until the end.
func TestJailToolRouteStreamsIncrementally(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseBackend := func() { once.Do(func() { close(release) }) }
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: two\n\n")
	}))
	b := New(testConfig(t, withTimeouts(TimeoutConfig{Control: 100 * time.Millisecond})))
	_ = b.reg.Register(regTool("sse", up.URL, "read"))
	srv := jailServer(t, b)
	// LIFO cleanups: release the blocked backend handler FIRST — both Close
	// calls wait for the in-flight request it holds.
	t.Cleanup(up.Close)
	t.Cleanup(srv.Close)
	t.Cleanup(releaseBackend)

	conn := dialJail(t, b, srv, "worker")
	fmt.Fprintf(conn, "POST /mcp/sse/ HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		e2eServerName, len(initializeMsg), initializeMsg)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response headers before the backend finished (response is buffered): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := bufio.NewReader(resp.Body)
	readLineContaining(t, body, "data: one", "first event before the backend finished")
	releaseBackend()
	readLineContaining(t, body, "data: two", "stream tail after the backend finished")
}

// A slow-but-successful backend completes with 200 whatever the control
// bound: tool routes are not handler-bounded.
func TestJailToolRouteSlowBackendCompletes(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer up.Close()
	b := New(testConfig(t, withTimeouts(TimeoutConfig{Control: 300 * time.Millisecond})))
	_ = b.reg.Register(regTool("slow", up.URL, "read"))
	h := b.JailHandler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp/slow/", strings.NewReader(initializeMsg))
	req.TLS = leafFor(t, b, "worker")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a tool call must not be cut by a handler bound): %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"result"`) {
		t.Fatalf("backend answer not delivered: %q", rec.Body.String())
	}
}

// slowStartRuntime is a fakeRuntime whose Start takes delay: a scion start
// that legitimately outlives the control bound.
type slowStartRuntime struct {
	*fakeRuntime
	delay time.Duration
}

func (s *slowStartRuntime) Start(ctx context.Context, o scion.StartOpts) error {
	time.Sleep(s.delay)
	return s.fakeRuntime.Start(ctx, o)
}

// A worker dispatch that outlives the control bound still completes with
// 200: the /worker/* verbs are not handler-bounded (a start waits for scion
// and the liveness settle window).
func TestJailWorkerRouteSlowStartCompletes(t *testing.T) {
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker",
		HostWorkspace: filepath.Join(t.TempDir(), "workers", "worker"),
		TicketDir:     "/run/user/501/lever/tickets/worker"}
	rt := &slowStartRuntime{fakeRuntime: &fakeRuntime{agents: map[string][]scion.Agent{}}, delay: 400 * time.Millisecond}
	b := New(testConfig(t, withManager("test-manager", ""), withRuntime(rt, spec),
		withTimeouts(TimeoutConfig{Control: 100 * time.Millisecond})))

	rec := callWorker(t, b, "/worker/start", `{"worker":"worker","task":"do it"}`, "test-manager")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a dispatch must not be cut by a handler bound): %s", rec.Code, rec.Body.String())
	}
	if len(rt.started) != 1 {
		t.Fatalf("start calls = %d, want 1", len(rt.started))
	}
}

// /llm is exempt from the handler deadline: a streamed completion outlives
// the control bound and is delivered whole.
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
	short := TimeoutConfig{Control: 100 * time.Millisecond}
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

	conn := dialJail(t, b, srv, "worker")
	// Headers promise 1000 bytes; deliver one and stall.
	fmt.Fprintf(conn, "POST /llm/v1/messages HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{", e2eServerName, tok)
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
