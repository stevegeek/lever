package broker

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	up       *httptest.Server
	upBodies *[]string
	cache    string // the tool's discovery copy
	// session, when set, is the Mcp-Session-Id the upstream's initialize
	// answer opens.
	session *string
}

func newServiceHostRig(t *testing.T, transportPin *servicehost.Fingerprint) *serviceHostRig {
	t.Helper()
	r := &serviceHostRig{t: t, server: newPinnedPair(t, x509.ExtKeyUsageServerAuth), client: newPinnedPair(t, x509.ExtKeyUsageClientAuth), shLog: &lockedBuf{}}
	var mu sync.Mutex
	bodies := []string{}
	r.upBodies = &bodies
	session := ""
	r.session = &session
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		sid := session
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(string(b), `"method":"initialize"`):
			if sid != "" {
				w.Header().Set("Mcp-Session-Id", sid)
			}
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"fake-db","version":"1"}}}`)
		case strings.Contains(string(b), `"method":"tools/list"`):
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"read","inputSchema":{"type":"object","properties":{"table":{"type":"string"}}}}]}}`)
		case strings.Contains(string(b), `"method":"notifications/`):
			w.WriteHeader(http.StatusAccepted)
		default:
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"from the service host"}]}}`)
		}
	}))
	t.Cleanup(up.Close)
	r.up = up
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
	r.cache = filepath.Join(t.TempDir(), "service-hosts", "laptop", "discovery-db.json")
	cfg.ServiceHosts = map[string]ServiceHostRoute{"db": {Host: "laptop", Transport: servicehost.Transport(r.client.cert, pin), DiscoveryCache: r.cache}}
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

func TestGatewayRefusesKeysEqualUnderCaseFolding(t *testing.T) {
	r := newServiceHostRig(t, nil)
	tok, err := token.Mint(r.b.keys.Private, token.Grant{
		Agent: "worker", Capability: token.Capability{Tool: "db", Operation: "read"}, Expiry: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	g := &gatewayRig{t: t, b: r.b, tool: "db"}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"_capability":"` +
		base64urlNoPad(tok) + `"}},"paramſ":{"name":"write"}}`
	if w := g.post("worker", body); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	// An exact duplicate too: a backend that keeps the first "method" would
	// run a tools/call the broker read as tools/list.
	dup := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write"},"method":"tools/list"}`
	if w := g.post("worker", dup); w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate method: status = %d, want 400", w.Code)
	}
	if len(*r.upBodies) != 0 {
		t.Fatal("SECURITY: a folded-key message reached the backend")
	}
}

func TestServiceHostCallerSurvivesAConnectionHeader(t *testing.T) {
	r := newServiceHostRig(t, nil)
	tok, err := token.Mint(r.b.keys.Private, token.Grant{
		Agent: "worker", Capability: token.Capability{Tool: "db", Operation: "read"}, Expiry: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/mcp/db/", strings.NewReader(toolsCall("read", `"table":"A"`, base64urlNoPad(tok))))
	req.Header.Set("Connection", "X-Lever-Caller")
	req.TLS = leafFor(t, r.b, "worker")
	h, err := r.b.gatewayHandler("db")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(r.shLog.String(), "caller=worker") {
		t.Fatalf("status %d; service host log:\n%s", w.Code, r.shLog)
	}
}

func (r *serviceHostRig) post(body string) *httptest.ResponseRecorder {
	r.t.Helper()
	g := &gatewayRig{t: r.t, b: r.b, tool: "db"}
	return g.post("worker", body)
}

const (
	hostInitializeMsg  = `{"jsonrpc":"2.0","id":%s,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code","version":"2"}}}`
	hostToolsListMsg   = `{"jsonrpc":"2.0","id":%s,"method":"tools/list"}`
	hostInitializedMsg = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
)

// While the service host is away, initialize and tools/list are answered
// from the last good answers (kept on disk, so a broker restart keeps them),
// with the request's id and the capability argument; tools/call still gets
// the 503, and works again once the host is back.
func TestServiceHostDiscoveryServedFromCopyWhileAway(t *testing.T) {
	r := newServiceHostRig(t, nil)
	if w := r.post(fmt.Sprintf(hostInitializeMsg, "1")); w.Code != http.StatusOK {
		t.Fatalf("live initialize: %d %s", w.Code, w.Body)
	}
	live := r.post(fmt.Sprintf(hostToolsListMsg, "2"))
	if live.Code != http.StatusOK || !strings.Contains(live.Body.String(), "_capability") {
		t.Fatalf("live tools/list: %d %s", live.Code, live.Body)
	}
	fi, err := os.Stat(r.cache)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the copy on disk: %v %v", fi, err)
	}

	r.ts.Close()
	// gatewayHandler is built again for every post: this is also a broker
	// restart reading the copy from disk.
	w := r.post(fmt.Sprintf(hostInitializeMsg, `"init-7"`))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"init-7"`) || !strings.Contains(w.Body.String(), `"fake-db"`) {
		t.Fatalf("initialize while away: %d %s", w.Code, w.Body)
	}
	if w := r.post(hostInitializedMsg); w.Code != http.StatusAccepted {
		t.Fatalf("notifications/initialized while away: %d %s", w.Code, w.Body)
	}
	w = r.post(fmt.Sprintf(hostToolsListMsg, "12345678901234567890"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":12345678901234567890`) ||
		!strings.Contains(w.Body.String(), `"read"`) || !strings.Contains(w.Body.String(), "_capability") {
		t.Fatalf("tools/list while away: %d %s", w.Code, w.Body)
	}
	if w := r.call("read"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("tools/call while away: %d, want 503", w.Code)
	}
	if !strings.Contains(r.audit.String(), "gateway.discovery_from_copy") {
		t.Fatalf("serving from the copy is not logged:\n%s", r.audit)
	}

	ln, err := net.Listen("tcp", r.addr)
	if err != nil {
		t.Skipf("cannot listen on %s again: %v", r.addr, err)
	}
	r.start(ln)
	if w := r.call("read"); w.Code != http.StatusOK {
		t.Fatalf("after the host is back: %d %s", w.Code, w.Body)
	}
}

// Without a copy, discovery fails as before.
func TestServiceHostDiscoveryWithoutACopyFails(t *testing.T) {
	r := newServiceHostRig(t, nil)
	r.ts.Close()
	if w := r.post(fmt.Sprintf(hostInitializeMsg, "1")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("initialize with no copy: %d, want 503", w.Code)
	}
	if w := r.post(hostInitializedMsg); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("notifications/initialized with no copy: %d, want 503", w.Code)
	}
}

// The host is up but its tool's server is down (the host answers 503):
// discovery comes from the copy too.
func TestServiceHostDiscoveryFromCopyWhenTheToolIsDown(t *testing.T) {
	r := newServiceHostRig(t, nil)
	r.post(fmt.Sprintf(hostInitializeMsg, "1"))
	r.post(fmt.Sprintf(hostToolsListMsg, "2"))
	r.up.Close()
	// The whole connect sequence must succeed: a client fails its connect
	// if the notification is not accepted.
	if w := r.post(fmt.Sprintf(hostInitializeMsg, "3")); w.Code != http.StatusOK {
		t.Fatalf("initialize with the tool down: %d %s", w.Code, w.Body)
	}
	if w := r.post(hostInitializedMsg); w.Code != http.StatusAccepted {
		t.Fatalf("notifications/initialized with the tool down: %d %s", w.Code, w.Body)
	}
	if w := r.post(fmt.Sprintf(hostToolsListMsg, "4")); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"read"`) {
		t.Fatalf("tools/list with the tool down: %d %s", w.Code, w.Body)
	}
}

// A server that opens sessions gets no copy: a copied initialize has no
// session, so later calls would be refused for it.
func TestServiceHostDiscoveryKeepsNoCopyForASessionServer(t *testing.T) {
	r := newServiceHostRig(t, nil)
	r.post(fmt.Sprintf(hostInitializeMsg, "1"))
	r.post(fmt.Sprintf(hostToolsListMsg, "2"))
	if _, err := os.Stat(r.cache); err != nil {
		t.Fatalf("no copy for a sessionless server: %v", err)
	}
	*r.session = "sess-1"
	if w := r.post(fmt.Sprintf(hostInitializeMsg, "3")); w.Code != http.StatusOK {
		t.Fatalf("live initialize: %d %s", w.Code, w.Body)
	}
	if raw, _ := os.ReadFile(r.cache); loadHostDiscovery(r.cache).has() || strings.Contains(string(raw), "fake-db") {
		t.Fatalf("the copy must be dropped once the server opens a session: %s", raw)
	}
	// Once seen opening a session, the server gets no copy again, also
	// after a broker restart (the handler is rebuilt for every post).
	*r.session = ""
	r.post(fmt.Sprintf(hostInitializeMsg, "5"))
	r.post(fmt.Sprintf(hostToolsListMsg, "6"))
	r.ts.Close()
	if w := r.post(fmt.Sprintf(hostInitializeMsg, "7")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("initialize of a session server while away: %d, want 503", w.Code)
	}
	if w := r.post(fmt.Sprintf(hostToolsListMsg, "8")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("tools/list of a session server while away: %d, want 503", w.Code)
	}
}

// An error answer is never kept.
func TestHostDiscoveryKeepsOnlyGoodAnswers(t *testing.T) {
	d := loadHostDiscovery(filepath.Join(t.TempDir(), "d.json"))
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"boom"}}`,
		`{"jsonrpc":"2.0","id":1,"result":[1]}`,
		`[{"jsonrpc":"2.0","id":1,"result":{}}]`,
		`not json`,
	} {
		if err := d.record(discoveryRequest{method: "tools/list", id: json.RawMessage("1")}, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if d.has() {
		t.Fatal("a bad answer was kept")
	}
	if _, ok := d.answer(discoveryRequest{method: "tools/list", id: json.RawMessage("1")}); ok {
		t.Fatal("answered without a copy")
	}
}

// The copy is shared by every agent, so what one agent asks must not shape
// it: a later page of tools/list is neither kept nor served, and an
// initialize is kept and served only for the protocolVersion it answers.
func TestHostDiscoveryIgnoresPagesAndOtherVersions(t *testing.T) {
	d := loadHostDiscovery(filepath.Join(t.TempDir(), "d.json"))
	req := func(method, body string) discoveryRequest { return newDiscoveryRequest(method, []byte(body)) }
	page2 := req("tools/list", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":"p2"}}`)
	if err := d.record(page2, []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"x"}]}}`)); err != nil {
		t.Fatal(err)
	}
	first := req("tools/list", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if err := d.record(first, []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a"}],"nextCursor":"p2"}}`)); err != nil {
		t.Fatal(err)
	}
	if d.has() {
		t.Fatal("a page of a paginated list was kept")
	}
	old := req("initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	if err := d.record(old, []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`)); err != nil {
		t.Fatal(err)
	}
	if d.has() {
		t.Fatal("an initialize answering another version than asked was kept")
	}
	cur := req("initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if err := d.record(cur, []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.answer(cur); !ok {
		t.Fatal("the matching version is not served")
	}
	if _, ok := d.answer(old); ok {
		t.Fatal("a copy was served to a client asking another protocolVersion")
	}
	noID := req("initialize", `{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if _, ok := d.answer(noID); ok {
		t.Fatal("a request without an id was answered")
	}
	// Read back from disk: the copy survives a restart.
	if _, ok := loadHostDiscovery(d.path).answer(cur); !ok {
		t.Fatal("the copy did not survive a reload")
	}
}
