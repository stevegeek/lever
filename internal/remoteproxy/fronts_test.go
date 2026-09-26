package remoteproxy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
)

// Tests for the non-Tailscale-front knobs (issue #38): a configurable
// identity header, X-Forwarded-Host, and a non-loopback bind.

const exeHeader = "X-ExeDev-Email"

// frontProbe sends one GET /api/v1/agents through a handler built from cfg
// (Target and Session filled in), with hdr added — a repeated key sends every
// value — and returns the status and the hub it went to.
func frontProbe(t *testing.T, cfg Config, host string, hdr http.Header) (int, *recordingHub) {
	t.Helper()
	hub := newRecordingHub(t)
	cfg.Target, cfg.Session = mustURL(t, hub.URL), testSession()
	if cfg.ServeHost == "" {
		cfg.ServeHost = testServeHost
	}
	req := proxyRequest("GET", "/api/v1/agents", nil)
	if host != "" {
		req.Host = host
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rw := httptest.NewRecorder()
	NewHandler(cfg).ServeHTTP(rw, req)
	return rw.Code, hub
}

// The configured header, not Tailscale's, decides allowed_users — and a
// Tailscale-User-Login a client adds is then just another header.
func TestIdentityHeaderDecidesAllowedUsers(t *testing.T) {
	cfg := Config{IdentityHeader: exeHeader, AllowedUsers: []string{"me@example.com"}}
	for _, tc := range []struct {
		name string
		hdr  http.Header
		want int
	}{
		{"the configured header, allowed", http.Header{exeHeader: {"me@example.com"}}, 200},
		{"lowercase on the wire is the same header", http.Header{"x-exedev-email": {"me@example.com"}}, 200},
		{"the configured header, another login", http.Header{exeHeader: {"z@example.com"}}, 403},
		{"no header", nil, 403},
		{"only Tailscale's header", http.Header{"Tailscale-User-Login": {"me@example.com"}}, 403},
		// A front that APPENDS instead of overwriting yields either shape;
		// both are refused rather than resolved first-value-wins.
		{"the header twice", http.Header{exeHeader: {"me@example.com", "z@example.com"}}, 403},
		{"the header twice, forged second", http.Header{exeHeader: {"z@example.com", "me@example.com"}}, 403},
		{"a comma-joined value", http.Header{exeHeader: {"me@example.com, z@example.com"}}, 403},
		{"a comma-joined value, no space", http.Header{exeHeader: {"z@example.com,me@example.com"}}, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, hub := frontProbe(t, cfg, "", tc.hdr)
			if status != tc.want {
				t.Fatalf("status = %d, want %d", status, tc.want)
			}
			if tc.want != 200 && hub.hits() != 0 {
				t.Fatal("a refused request reached the hub")
			}
		})
	}
}

// With the default header, duplicates and comma-joined values are refused
// too — the rule is the gate's, not the front's. The denial is the comma
// check itself, not the allowlist miss that would follow it: here the first
// value IS allowed, so only the comma rule can refuse.
func TestDefaultHeaderRefusesCommaJoinedLogin(t *testing.T) {
	for _, v := range []string{"a@github,b@github", "b@github, a@github"} {
		var lines []AuditLine
		hub := newRecordingHub(t)
		h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(), ServeHost: testServeHost,
			AllowedUsers: []string{"a@github", "a@github,b@github", "b@github, a@github"}, Audit: func(l AuditLine) { lines = append(lines, l) }})
		req := proxyRequest("GET", "/api/v1/agents", nil)
		req.Header.Set("Tailscale-User-Login", v)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusForbidden || hub.hits() != 0 {
			t.Fatalf("%q: status = %d, hits = %d; want 403 before the hub", v, rw.Code, hub.hits())
		}
		if !strings.Contains(rw.Body.String(), "comma-joined") || len(lines) != 1 || lines[0].Decision != DecisionDenyUser {
			t.Fatalf("%q: want the comma-joined denial, got %q / %+v", v, rw.Body.String(), lines)
		}
	}
}

// The asserted hub identity comes from the configured header: the login
// driver is asked for exactly that login.
func TestIdentityHeaderIsTheAssertedOperator(t *testing.T) {
	hub := newRecordingHub(t)
	sess := testSession()
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: sess, ServeHost: testServeHost,
		IdentityHeader: exeHeader, AllowedUsers: []string{"me@example.com"}})
	req := proxyRequest("GET", "/api/v1/agents", nil)
	req.Header.Set(exeHeader, "me@example.com")
	req.Header.Set("Tailscale-User-Login", "someone-else@github")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 200 {
		t.Fatalf("status = %d", rw.Code)
	}
	sess.mu.Lock()
	got := append([]string(nil), sess.logins...)
	sess.mu.Unlock()
	if len(got) != 1 || got[0] != "me@example.com" {
		t.Fatalf("session asked for %v, want [me@example.com]", got)
	}
}

// The configured header is stripped before the hub sees the request, along
// with Tailscale-* and the rest of clientIdentityHeader's set.
func TestIdentityHeaderStrippedFromForward(t *testing.T) {
	status, hub := frontProbe(t, Config{IdentityHeader: exeHeader, AllowedUsers: []string{"me@example.com"}}, "",
		http.Header{exeHeader: {"me@example.com"}, "Tailscale-User-Login": {"x@github"}, "X-Scion-Agent-Token": {"t"}})
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	for _, k := range []string{exeHeader, "Tailscale-User-Login", "X-Scion-Agent-Token"} {
		if v := hub.lastHeader().Values(k); len(v) != 0 {
			t.Fatalf("%s reached the hub: %q", k, v)
		}
	}
	if c := hub.lastHeader().Get("Cookie"); c != sessionCookieName+"="+testCookie {
		t.Fatalf("Cookie = %q, want only the operator's session", c)
	}
}

// X-Forwarded-Host is ignored unless the operator opted in: a rebinding page
// could otherwise name base_url's host in it.
func TestForwardedHostIgnoredByDefault(t *testing.T) {
	for _, host := range []string{"internal-name:8445", "10.1.2.3:9000", "127.0.0.1"} {
		// An IP-literal Host is exactly the case the opt-in would admit, so
		// it is the one that shows the default really ignores the header.
		status, hub := frontProbe(t, Config{ListenPort: 8445}, host,
			http.Header{"X-Forwarded-Host": {testServeHost}})
		if status != http.StatusForbidden || hub.hits() != 0 {
			t.Fatalf("Host %s: status = %d, hits = %d; X-Forwarded-Host must not replace Host by default", host, status, hub.hits())
		}
	}
	// And the default does not judge the header at all: repeated or
	// comma-joined values on an otherwise good request are not refused.
	for _, xfh := range [][]string{{"a.example", "b.example"}, {"a.example, b.example"}} {
		if status, _ := frontProbe(t, Config{ListenPort: 8445}, "", http.Header{"X-Forwarded-Host": xfh}); status != 200 {
			t.Fatalf("X-Forwarded-Host %q with the flag off: status %d, want 200", xfh, status)
		}
	}
}

func TestForwardedHostOptIn(t *testing.T) {
	cfg := Config{ListenPort: 8445, TrustForwardedHost: true}
	for _, tc := range []struct {
		name, host string
		hdr        http.Header
		want       int
	}{
		{"front rewrote Host to an IP, passes the original", "10.1.2.3:9000", http.Header{"X-Forwarded-Host": {testServeHost}}, 200},
		{"front rewrote Host to a bare IP", "127.0.0.1", http.Header{"X-Forwarded-Host": {testServeHost}}, 200},
		{"front rewrote Host to an IPv6 literal", "[::1]:9000", http.Header{"X-Forwarded-Host": {testServeHost}}, 200},
		{"forwarded host is judged like Host, case-insensitively", "127.0.0.1", http.Header{"X-Forwarded-Host": {"MAC.ts.net"}}, 200},
		{"forwarded host names another site", "127.0.0.1", http.Header{"X-Forwarded-Host": {"evil.example"}}, 403},
		{"two forwarded hosts", "127.0.0.1", http.Header{"X-Forwarded-Host": {testServeHost, "evil.example"}}, 403},
		{"comma-joined forwarded hosts", "127.0.0.1", http.Header{"X-Forwarded-Host": {"evil.example, " + testServeHost}}, 403},
		// The 2026-08-22 rebind, with the header added: the browser sends
		// the attacker's NAME in Host, so the header is not believed.
		{"rebound name with a forged forwarded host", "evil.example:8445", http.Header{"X-Forwarded-Host": {testServeHost}}, 403},
		{"localhost is a name, not an IP", "localhost:8445", http.Header{"X-Forwarded-Host": {testServeHost}}, 403},
		{"a rewritten name is not supported", "internal-name:8445", http.Header{"X-Forwarded-Host": {testServeHost}}, 403},
		// Without the header the ordinary Host check applies: the loopback
		// doctor probe still passes, a rebound name still does not.
		{"no header, loopback probe", "127.0.0.1:8445", nil, 200},
		{"no header, base_url host", testServeHost, nil, 200},
		{"no header, rebound name", "evil.example", nil, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, hub := frontProbe(t, cfg, tc.host, tc.hdr)
			if status != tc.want {
				t.Fatalf("status = %d, want %d", status, tc.want)
			}
			if tc.want != 200 && hub.hits() != 0 {
				t.Fatal("a refused request reached the hub")
			}
		})
	}
}

// A non-loopback bind adds exactly "<bind>:<port>" to the Host allowlist:
// the Host a front sends when it dials that address.
func TestBindHostAdmittedOnlyOnItsPort(t *testing.T) {
	cfg := Config{ListenPort: 8445, BindHost: "10.0.0.5"}
	for host, want := range map[string]int{
		"10.0.0.5:8445":  200,
		"10.0.0.5:9999":  403,
		"10.0.0.5":       403,
		"10.0.0.6:8445":  403,
		"127.0.0.1:8445": 200,
		"evil.example":   403,
	} {
		t.Run(host, func(t *testing.T) {
			if status, _ := frontProbe(t, cfg, host, nil); status != want {
				t.Fatalf("Host %q → %d, want %d", host, status, want)
			}
		})
	}
	// With no bind host configured, the address is just another name.
	if status, _ := frontProbe(t, Config{ListenPort: 8445}, "10.0.0.5:8445", nil); status != http.StatusForbidden {
		t.Fatalf("a bind address must not be admitted when the proxy is not bound to it, got %d", status)
	}
	// A wildcard is never a Host.
	if status, _ := frontProbe(t, Config{ListenPort: 8445, BindHost: "0.0.0.0"}, "0.0.0.0:8445", nil); status != http.StatusForbidden {
		t.Fatalf("0.0.0.0 must never be an admitted Host, got %d", status)
	}
}

// A login without "@" (a front's user id) becomes a distinct synthesized hub
// email; an email stays itself; HubUserEmails agrees with identityFor, so the
// remote role binds the users the proxy signs in.
func TestIdentityForUserIDs(t *testing.T) {
	if got := identityFor("usr_01HZX"); got.Email != "usr_01HZX@"+IDEmailDomain || got.Name != "usr_01HZX" || got.Subject != "lever-remote:usr_01HZX" {
		t.Fatalf("identityFor(user id) = %+v", got)
	}
	if got := identityFor("me@example.com"); got.Email != "me@example.com" {
		t.Fatalf("identityFor(email) = %+v", got)
	}
	got := HubUserEmails([]string{"me@example.com", "usr_01HZX"})
	if len(got) != 2 || got[0] != "me@example.com" || got[1] != "usr_01HZX@"+IDEmailDomain {
		t.Fatalf("HubUserEmails = %v", got)
	}
}

// config refuses allowed_users entries that would alias the synthesized
// addresses; that only works while both packages name the same domain and
// placeholder.
func TestIDEmailDomainAgreesWithConfig(t *testing.T) {
	base := "name: x\nbackend: orbstack\ntree: ./tree\nmanager: {}\nremote:\n  enabled: true\n  base_url: \"https://h.ts.net\"\n  allowed_users: [%q]\n"
	for _, addr := range []string{"alice@" + IDEmailDomain, unnamedOperatorEmail} {
		dir := t.TempDir()
		p := writeLeverYAML(t, dir, strings.Replace(base, "%q", strconv.Quote(addr), 1))
		if _, err := config.LoadNoHostChecks(p); err == nil || !strings.Contains(err.Error(), "allowed_users") {
			t.Fatalf("config accepted %q (err=%v); its synthesized-address guard has drifted from remoteproxy", addr, err)
		}
	}
}

// Every header the proxy strips as a client identity, other than
// Tailscale-User-Login (the default), must be refused by config as an
// identity header — trusting one would let a browser, a scion-facing caller
// or the user's own display settings choose.
func TestConfigRefusesStrippedIdentityHeaders(t *testing.T) {
	for _, h := range []string{"Authorization", "Cookie", "X-Scion-Agent-Token", "X-Scion-Broker-Signature",
		"X-Forwarded-User-Email", "X-Goog-Iap-Jwt-Assertion", "X-Api-Key",
		// Tailscale-* is stripped too, and only its User-Login is a unique
		// login; the display name and avatar must be refused.
		"Tailscale-User-Name", "Tailscale-User-Profile-Pic"} {
		if h != "Authorization" && h != "Cookie" && !clientIdentityHeader(h) {
			t.Fatalf("test premise: %s is not a stripped header", h)
		}
		dir := t.TempDir()
		p := writeLeverYAML(t, dir, "name: x\nbackend: orbstack\ntree: ./tree\nmanager: {}\nremote:\n  enabled: true\n"+
			"  base_url: \"https://h.ts.net\"\n  identity_header: "+h+"\n")
		if _, err := config.LoadNoHostChecks(p); err == nil || !strings.Contains(err.Error(), "identity_header") {
			t.Fatalf("config accepted identity_header %s (err=%v)", h, err)
		}
	}
}

// listenProxy: loopback by default; a non-loopback address only with the
// acknowledgement; never a hostname.
func TestListenProxyBind(t *testing.T) {
	port := freeTCPPort(t)
	ln, err := listenProxy("", port, false)
	if err != nil {
		t.Fatalf("default bind: %v", err)
	}
	if !ln.Addr().(*net.TCPAddr).IP.IsLoopback() {
		t.Fatalf("default bind is %s, want loopback", ln.Addr())
	}
	_ = ln.Close()

	ln, err = listenProxy("127.0.0.1", port, false)
	if err != nil {
		t.Fatalf("explicit loopback: %v", err)
	}
	_ = ln.Close()

	if _, err := listenProxy("0.0.0.0", port, false); err == nil {
		t.Fatal("a non-loopback bind without the acknowledgement must be refused")
	}
	if _, err := listenProxy("localhost", port, true); err == nil {
		t.Fatal("a hostname bind must be refused")
	}
	// The wildcard with the acknowledgement binds (every interface, so this
	// works on any test host).
	ln, err = listenProxy("0.0.0.0", port, true)
	if err != nil {
		t.Fatalf("acknowledged wildcard: %v", err)
	}
	_ = ln.Close()
}

// writeLeverYAML writes a minimal instance (the tree directory included) with
// body as its lever.yaml, defaulting llm_auth to subscription so no api key
// file is needed, and returns the config path.
func writeLeverYAML(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "tree"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "lever.yaml")
	if err := os.WriteFile(p, []byte(body+"broker:\n  llm_auth: subscription\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
