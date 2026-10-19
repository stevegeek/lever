package broker

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/servicehost"
)

type pinnedPair struct {
	cert tls.Certificate
	fp   servicehost.Fingerprint
}

func newPinnedPair(t *testing.T, eku x509.ExtKeyUsage) pinnedPair {
	t.Helper()
	c, k, err := servicehost.GenerateSelfSigned("test", eku)
	if err != nil {
		t.Fatal(err)
	}
	cert, fp, err := servicehost.LoadKeyPair(c, k)
	if err != nil {
		t.Fatal(err)
	}
	return pinnedPair{cert, fp}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// serviceHostRig is a broker whose tool "db" runs on a service host: a real
// servicehost.Server behind TLS on a fixed address, in front of an upstream
// MCP server that records what reaches it.
type serviceHostRig struct {
	t        *testing.T
	b        *Broker
	audit    *bytes.Buffer
	shLog    *lockedBuf
	sh       *servicehost.Server
	server   pinnedPair
	client   pinnedPair
	addr     string
	ts       *httptest.Server
	upBodies *[]string
}

func newServiceHostRig(t *testing.T, transportPin *servicehost.Fingerprint) *serviceHostRig {
	t.Helper()
	r := &serviceHostRig{t: t, server: newPinnedPair(t, x509.ExtKeyUsageServerAuth), client: newPinnedPair(t, x509.ExtKeyUsageClientAuth), shLog: &lockedBuf{}}
	var mu sync.Mutex
	bodies := []string{}
	r.upBodies = &bodies
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"from the service host"}]}}`)
	}))
	t.Cleanup(up.Close)
	sh, err := servicehost.NewServer(&servicehost.Config{
		Listen: "127.0.0.1:1", ClientFingerprint: string(r.client.fp),
		Tools: []servicehost.Tool{{Name: "db", Upstream: up.URL, Allow: []string{"read"}}},
	}, slog.New(slog.NewTextHandler(r.shLog, nil)), "test")
	if err != nil {
		t.Fatal(err)
	}
	r.sh = sh
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.addr = ln.Addr().String()
	r.start(ln)

	pin := r.server.fp
	if transportPin != nil {
		pin = *transportPin
	}
	cfg, audit := auditConfig(t)
	r.audit = audit
	cfg.ServiceHosts = map[string]ServiceHostRoute{"db": {Host: "laptop", Transport: servicehost.Transport(r.client.cert, pin)}}
	r.b = New(cfg)
	tool := registry.Tool{Name: "db", Backend: servicehost.ToolURL(r.addr, "db"), External: true,
		Operations: map[string]registry.Operation{"read": {Name: "read"}, "write": {Name: "write"}}}
	if err := r.b.reg.Register(tool); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *serviceHostRig) start(ln net.Listener) {
	ts := httptest.NewUnstartedServer(r.sh)
	ts.Listener.Close()
	ts.Listener = ln
	ts.TLS = servicehost.ServerTLSConfig(r.server.cert, r.client.fp)
	ts.Config.ErrorLog = slog.NewLogLogger(slog.New(slog.NewTextHandler(r.shLog, nil)).Handler(), slog.LevelWarn)
	ts.StartTLS()
	r.ts = ts
	r.t.Cleanup(ts.Close)
}

func (r *serviceHostRig) call(op string) *httptest.ResponseRecorder {
	r.t.Helper()
	tok, err := token.Mint(r.b.keys.Private, token.Grant{
		Agent: "worker", Capability: token.Capability{Tool: "db", Operation: op},
		Expiry: time.Now().Add(time.Hour),
	})
	if err != nil {
		r.t.Fatal(err)
	}
	g := &gatewayRig{t: r.t, b: r.b, tool: "db"}
	return g.post("worker", toolsCall(op, `"table":"A"`, base64urlNoPad(tok)))
}

func TestGatewayReachesServiceHostOverPinnedMutualTLS(t *testing.T) {
	r := newServiceHostRig(t, nil)
	w := r.call("read")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "from the service host") {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if len(*r.upBodies) != 1 || strings.Contains((*r.upBodies)[0], "_capability") {
		t.Fatalf("SECURITY: upstream saw %v (the capability must be stripped)", *r.upBodies)
	}
	if !strings.Contains(r.shLog.String(), "decision=allow tool=db op=read caller=worker") {
		t.Fatalf("the service host did not audit the call with its caller:\n%s", r.shLog)
	}
}

func TestServiceHostCeilingRefusesWhatTheBrokerAllows(t *testing.T) {
	r := newServiceHostRig(t, nil)
	w := r.call("write")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 from the ceiling; body = %s", w.Code, w.Body)
	}
	if len(*r.upBodies) != 0 {
		t.Fatalf("SECURITY: a call outside the ceiling reached the upstream: %v", *r.upBodies)
	}
	out := r.audit.String()
	if !strings.Contains(out, "decision=allow") || !strings.Contains(out, "service_host.refused") || !strings.Contains(out, "host=laptop") {
		t.Fatalf("the broker's log lacks the allow and the service host's refusal:\n%s", out)
	}
	if !strings.Contains(r.shLog.String(), "decision=deny tool=db op=write") {
		t.Fatalf("the service host did not audit its refusal:\n%s", r.shLog)
	}
}

func TestServiceHostDownThenBack(t *testing.T) {
	r := newServiceHostRig(t, nil)
	if w := r.call("read"); w.Code != http.StatusOK {
		t.Fatalf("before: %d %s", w.Code, w.Body)
	}
	r.ts.Close()
	w := r.call("read")
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), r.addr) {
		t.Fatalf("down: %d %q, want 503 without the address", w.Code, w.Body)
	}
	ln, err := net.Listen("tcp", r.addr)
	if err != nil {
		t.Skipf("cannot listen on %s again: %v", r.addr, err)
	}
	r.start(ln)
	if w := r.call("read"); w.Code != http.StatusOK {
		t.Fatalf("after it is back: %d %s, want the very next call to work", w.Code, w.Body)
	}
}

func TestServiceHostWithAnotherKeyIsRefused(t *testing.T) {
	impostor := newPinnedPair(t, x509.ExtKeyUsageServerAuth).fp
	r := newServiceHostRig(t, &impostor)
	w := r.call("read")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if len(*r.upBodies) != 0 {
		t.Fatal("SECURITY: a call went to a server whose key is not pinned")
	}
	if !strings.Contains(r.audit.String(), "pin mismatch") {
		t.Fatalf("the broker's log does not name the pin mismatch:\n%s", r.audit)
	}
}
