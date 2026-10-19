package broker

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"strconv"
	"sync"
	"testing"

	"github.com/stevegeek/lever/internal/broker/registry"
)

// hit is one request a pathUpstream saw.
type hit struct{ host, uri string }

// pathUpstream records every request URI and Host it is reached with. It
// stands for a backend host that serves other routes besides MCP.
type pathUpstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits []hit
}

func newPathUpstream(t *testing.T) *pathUpstream {
	t.Helper()
	u := &pathUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits = append(u.hits, hit{host: r.Host, uri: r.RequestURI})
		u.mu.Unlock()
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *pathUpstream) take() []hit {
	u.mu.Lock()
	defer u.mu.Unlock()
	h := u.hits
	u.hits = nil
	return h
}

// jailFront serves b's real jail mux over a real HTTP/1.1 listener, so the
// request line is parsed and the mux cleans paths exactly as in production.
// Every connection is the given caller (nil: no client leaf).
func jailFront(t *testing.T, b *Broker, caller string) string {
	t.Helper()
	jail := b.JailHandler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if caller != "" {
			r.TLS = leafFor(t, b, caller)
		}
		jail.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// rawPost writes target verbatim into the request line, which a Go client
// would normalise first.
func rawPost(t *testing.T, addr, target, host, body string) int {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = io.WriteString(c, "POST "+target+" HTTP/1.1\r\nHost: "+host+
		"\r\nContent-Type: application/json\r\nConnection: close\r\nContent-Length: "+
		strconv.Itoa(len(body))+"\r\n\r\n"+body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("%s: %v", target, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`

func pinRig(t *testing.T, mk func(backend string) registry.Tool, suffix string) (*Broker, *pathUpstream) {
	t.Helper()
	up := newPathUpstream(t)
	b := New(testConfig(t))
	if err := b.reg.Register(mk(strings.TrimPrefix(up.srv.URL, "http://") + suffix)); err != nil {
		t.Fatal(err)
	}
	return b, up
}

// The tool route reaches the backend's configured path, and only it, for an
// external tool with and without a path and for a first-party tool.
func TestGatewayPinsBackendPath(t *testing.T) {
	cases := []struct {
		name, suffix, want string
		mk                 func(string) registry.Tool
	}{
		{"external pathless", "", "/", func(b string) registry.Tool { return regTool("db", b, "read") }},
		{"external /mcp", "/mcp", "/mcp", func(b string) registry.Tool { return regTool("db", b, "read") }},
		{"external /mcp/", "/mcp/", "/mcp/", func(b string) registry.Tool { return regTool("db", b, "read") }},
		{"external query", "/mcp?k=v", "/mcp?k=v", func(b string) registry.Tool { return regTool("db", b, "read") }},
		{"first-party", "", "/", func(b string) registry.Tool { return firstPartyTool("db", b, "read") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, up := pinRig(t, c.mk, c.suffix)
			addr := jailFront(t, b, "worker")
			for _, target := range []string{"/mcp/db/", "/mcp/db/?x=1&sessionId=evil"} {
				if code := rawPost(t, addr, target, "broker", initBody); code != http.StatusOK {
					t.Fatalf("%s: status = %d, want 200", target, code)
				}
				hits := up.take()
				if len(hits) != 1 || hits[0].uri != c.want {
					t.Fatalf("%s reached the backend as %v, want exactly %q", target, hits, c.want)
				}
			}
		})
	}
}

// No inbound path other than the tool route reaches the backend anywhere but
// its configured path: other sub-paths 404, traversal and encoding tricks are
// refused or cleaned, and the Host header and an absolute-form target cannot
// redirect the request.
func TestGatewayRefusesOtherBackendPaths(t *testing.T) {
	b, up := pinRig(t, func(bk string) registry.Tool { return regTool("db", bk, "read") }, "/mcp")
	backendHost := strings.TrimPrefix(up.srv.URL, "http://")
	addr := jailFront(t, b, "worker")
	cap := mintFor(t, b, "worker", nil)
	call := toolsCall("read", `"table":"A"`, cap)

	for _, target := range []string{"/mcp/db/admin", "/mcp/db/metrics?x=1", "/mcp/db/mcp", "/mcp/db/x/y"} {
		for _, body := range []string{initBody, call} {
			if code := rawPost(t, addr, target, "broker", body); code != http.StatusNotFound {
				t.Fatalf("%s: status = %d, want 404", target, code)
			}
		}
	}
	for _, target := range []string{
		"/mcp/db/admin", "/mcp/db/../admin", "/mcp/db/%2e%2e/admin", "/mcp/db/%2E%2E/%2E%2E/admin",
		"/mcp/db/..%2fadmin", "/mcp/db/%2fadmin", "/mcp/db%2fadmin", "/mcp/db//admin", "//mcp/db/admin",
		"/mcp/db/./admin", "/mcp/db/%00", "/mcp/db/;admin", "/mcp/db/admin#x", "/mcp/db/%252e%252e/admin",
		"/mcp/db/.", "/mcp/db/..", "/mcp/db/%2e", "/mcp/db//", "/mcp//db/", "/mcp/db/./",
		"http://" + backendHost + "/admin", "http://evil.example/mcp/db/admin", "http://evil.example/mcp/db/",
		"*",
	} {
		for _, host := range []string{"broker", backendHost, "evil.example", "127.0.0.1:1"} {
			_ = rawPost(t, addr, target, host, initBody)
			for _, h := range up.take() {
				if h.uri != "/mcp" || h.host != backendHost {
					t.Fatalf("SECURITY: %s (Host %s) reached the backend as %s on Host %s", target, host, h.uri, h.host)
				}
			}
		}
	}
}

// A caller without a leaf gets the same 403 on every path: the path check
// tells it nothing about the backend.
func TestGatewayPathRefusalAfterAuth(t *testing.T) {
	b, up := pinRig(t, func(bk string) registry.Tool { return regTool("db", bk, "read") }, "")
	addr := jailFront(t, b, "")
	for _, target := range []string{"/mcp/db/", "/mcp/db/admin", "/mcp/db/x/y"} {
		if code := rawPost(t, addr, target, "broker", initBody); code != http.StatusForbidden {
			t.Fatalf("%s without a leaf: status = %d, want 403", target, code)
		}
	}
	if h := up.take(); len(h) != 0 {
		t.Fatalf("SECURITY: unauthenticated request reached the backend: %v", h)
	}
}

func TestGatewayAuditsRefusedPath(t *testing.T) {
	cfg, audit := auditConfig(t)
	b := New(cfg)
	up := newPathUpstream(t)
	if err := b.reg.Register(regTool("db", up.srv.URL, "read")); err != nil {
		t.Fatal(err)
	}
	addr := jailFront(t, b, "worker")
	if code := rawPost(t, addr, "/mcp/db/admin/"+strings.Repeat("a", 1000), "broker", initBody); code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	out := audit.String()
	if !strings.Contains(out, "decision=deny") || !strings.Contains(out, `detail="path not allowed"`) ||
		!strings.Contains(out, "caller=worker") || !strings.Contains(out, "path=/mcp/db/admin/aaa") {
		t.Fatalf("refused path not audited:\n%s", out)
	}
	if strings.Contains(out, strings.Repeat("a", maxAuditPath)) {
		t.Fatalf("audit path not truncated:\n%s", out)
	}
}
