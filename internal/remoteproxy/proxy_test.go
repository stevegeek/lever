package remoteproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", s, err)
	}
	return u
}

// TestPhoneCannotChooseItsIdentity: a client Authorization header is
// stripped (scion would let it pick the identity), and the client's Cookie
// is replaced by the session, never merged.
func TestPhoneCannotChooseItsIdentity(t *testing.T) {
	hub := newRecordingHub(t)
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(),
		ServeHost: "mac.ts.net"})
	for _, path := range []string{"/api/v1/agents", "/"} {
		req := proxyRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer attacker")
		req.Header.Add("Authorization", "Basic YTpi")
		req.Header.Set("Cookie", "scion_sess=evil")
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != 200 {
			t.Fatalf("GET %s: status %d", path, rw.Code)
		}
		if a := hub.lastHeader().Values("Authorization"); len(a) != 0 {
			t.Fatalf("GET %s: Authorization reached the hub: %q", path, a)
		}
		if c := hub.lastHeader().Get("Cookie"); c != sessionCookieName+"="+testCookie {
			t.Fatalf("GET %s: Cookie = %q, want only the operator's session", path, c)
		}
	}
}

// gateProbe returns the status and hub hit count for one GET through a
// handler; a repeated key in hdr sends every value.
func gateProbe(t *testing.T, serveHost string, hdr http.Header) (status, hubHits int) {
	t.Helper()
	hub := newRecordingHub(t)
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(), ServeHost: serveHost})
	req := proxyRequest("GET", "/api/v1/agents", nil)
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	return rw.Code, hub.hits()
}

func assertGateRefuses(t *testing.T, serveHost string, hdr http.Header) {
	t.Helper()
	status, hits := gateProbe(t, serveHost, hdr)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if hits != 0 {
		t.Fatalf("hub was hit %d times, want 0", hits)
	}
}

func assertGateForwards(t *testing.T, serveHost string, hdr http.Header) {
	t.Helper()
	status, hits := gateProbe(t, serveHost, hdr)
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if hits != 1 {
		t.Fatalf("hub hit count = %d, want 1", hits)
	}
}

func TestCrossOriginRejected(t *testing.T) {
	assertGateRefuses(t, "mac.ts.net", http.Header{"Origin": {"https://evil.example"}})
}

func TestSameOriginAllowed(t *testing.T) {
	assertGateForwards(t, "mac.ts.net", http.Header{"Origin": {"https://mac.ts.net"}})
}

// TestHTTPOriginRejected: the public origin is https, so http is another
// origin.
func TestHTTPOriginRejected(t *testing.T) {
	assertGateRefuses(t, "mac.ts.net", http.Header{"Origin": {"http://mac.ts.net"}})
}

func TestHubSPARequestAllowed(t *testing.T) {
	assertGateForwards(t, "mac.ts.net", http.Header{"Origin": {"https://mac.ts.net"}, "Sec-Fetch-Site": {"same-origin"}})
}

func TestSecFetchCrossSiteRejected(t *testing.T) {
	assertGateRefuses(t, "mac.ts.net", http.Header{"Sec-Fetch-Site": {"cross-site"}})
}

func TestHeaderFreeCurlAllowed(t *testing.T) {
	assertGateForwards(t, "mac.ts.net", nil) // no Origin, no Sec-Fetch-Site
}

func TestAllowedUsersPinning(t *testing.T) {
	cases := []struct {
		name       string
		allowed    []string
		loginHdr   string
		wantStatus int
	}{
		{"empty list, no header", nil, "", 200},
		{"empty list, header present, ignored", nil, "someone@example.com", 200},
		{"non-empty list, login in list", []string{"a@example.com", "b@example.com"}, "b@example.com", 200},
		{"non-empty list, login missing", []string{"a@example.com"}, "", http.StatusForbidden},
		{"non-empty list, login not in list", []string{"a@example.com"}, "z@example.com", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newRecordingHub(t)
			h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(),
				ServeHost: "mac.ts.net", AllowedUsers: tc.allowed})
			req := proxyRequest("GET", "/api/v1/agents", nil)
			if tc.loginHdr != "" {
				req.Header.Set("Tailscale-User-Login", tc.loginHdr)
			}
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, req)
			if rw.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rw.Code, tc.wantStatus)
			}
			wantHits := 0
			if tc.wantStatus == 200 {
				wantHits = 1
			}
			if n := hub.hits(); n != wantHits {
				t.Fatalf("hub hit count = %d, want %d", n, wantHits)
			}
		})
	}
}

func TestNoSessionSourceFailsClosed(t *testing.T) {
	hub := newRecordingHub(t)
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net"})
	for _, path := range []string{"/api/v1/agents", "/"} {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, proxyRequest("GET", path, nil))
		if rw.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET %s: status = %d, want 503", path, rw.Code)
		}
	}
	if n := hub.hits(); n != 0 {
		t.Fatalf("hub was hit %d times, want 0", n)
	}
}

// TestCredentialMintRoutesRefused: the injected session can mint hub
// credentials. /api/v1/auth/ is allowlisted, so unknown routes are refused
// too, while the SPA's token page keeps listing and revoking.
func TestCredentialMintRoutesRefused(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		refused      bool
	}{
		{"POST", "/api/v1/auth/tokens", true},
		{"POST", "/api/v1/auth/tokens/", true},
		{"POST", "/api/v1/auth/%74okens", true},
		{"POST", "/api/v1/auth/token", true},
		{"POST", "/api/v1/auth/refresh", true},
		{"POST", "/api/v1/auth/login", true},
		{"POST", "/api/v1/auth/cli/device", true},
		{"GET", "/api/v1/auth/cli/device/token", true},
		{"POST", "/api/v1/auth/cli/authorize", true},
		{"POST", "/api/v1/auth/integrations/google/exchange", true},
		{"POST", "/api/v1/auth/test-login", true},
		{"POST", "/api/v1/auth/validate", true},
		{"POST", "/api/v1/auth/invite/redeem", true},
		{"POST", "/api/v1/auth/some-future-route", true},
		{"GET", "/api/v1/auth", true},
		{"POST", "/api/v1/auth/tokens/t1", true},
		{"GET", "/api/v1/auth/tokens/t1/revoke", true},
		{"GET", "/api/v1/auth/tokens/t1", false},
		{"GET", "/api/v1/auth/admin-status", false},
		{"GET", "/api/v1/auth/scopes", false},
		{"GET", "/api/v1/auth/providers", false},
		{"POST", "/api/v1/auth/logout", false},
		{"POST", "/api/v1/authz/explain", false},
		{"GET", "/api/v1/auth/tokens", false},
		{"DELETE", "/api/v1/auth/tokens/t1", false},
		{"POST", "/api/v1/auth/tokens/t1/revoke", false},
		{"GET", "/api/v1/auth/me", false},
		{"POST", "/api/v1/agents/a1/messages", false},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			hub := newRecordingHub(t)
			var lines []AuditLine
			h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(),
				ServeHost: "mac.ts.net", Audit: func(l AuditLine) { lines = append(lines, l) }})
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, proxyRequest(tc.method, tc.path, nil))
			if tc.refused {
				if rw.Code != http.StatusForbidden || hub.hits() != 0 || len(lines) != 1 || lines[0].Decision != DecisionDenyMint {
					t.Fatalf("status %d, hub hits %d, audit %+v; want 403, 0, deny-credential-mint", rw.Code, hub.hits(), lines)
				}
				return
			}
			if rw.Code != 200 || hub.hits() != 1 {
				t.Fatalf("status %d, hub hits %d; want it forwarded", rw.Code, hub.hits())
			}
		})
	}
}

func TestAuditLineNeverCarriesTheSession(t *testing.T) {
	hub := newRecordingHub(t)
	var lines []AuditLine
	h := NewHandler(Config{
		Target:       mustURL(t, hub.URL),
		Session:      testSession(),
		ServeHost:    "mac.ts.net",
		AllowedUsers: []string{"a@example.com"},
		Audit:        func(line AuditLine) { lines = append(lines, line) },
	})

	reqs := []*http.Request{
		func() *http.Request {
			r := proxyRequest("GET", "/allow", nil)
			r.Header.Set("Tailscale-User-Login", "a@example.com")
			return r
		}(),
		func() *http.Request {
			r := proxyRequest("GET", "/deny-origin", nil)
			r.Header.Set("Origin", "https://evil.example")
			return r
		}(),
		func() *http.Request {
			r := proxyRequest("GET", "/deny-user", nil)
			r.Header.Set("Tailscale-User-Login", "z@example.com")
			return r
		}(),
	}
	for _, r := range reqs {
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	var noPatLines []AuditLine
	hNoPAT := NewHandler(Config{
		Target:    mustURL(t, hub.URL),
		Session:   &stubSession{err: errors.New("login down")},
		ServeHost: "mac.ts.net",
		Audit:     func(line AuditLine) { noPatLines = append(noPatLines, line) },
	})
	hNoPAT.ServeHTTP(httptest.NewRecorder(), proxyRequest("GET", "/deny-no-session", nil))

	if len(lines) != 3 {
		t.Fatalf("got %d audit lines for 3 requests, want 3 (one each)", len(lines))
	}
	if len(noPatLines) != 1 {
		t.Fatalf("got %d audit lines for 1 request, want 1", len(noPatLines))
	}
	wantDecisions := []Decision{DecisionAllow, DecisionDenyOrigin, DecisionDenyUser}
	for i, l := range lines {
		if l.Decision != wantDecisions[i] {
			t.Fatalf("line[%d].Decision = %q, want %q", i, l.Decision, wantDecisions[i])
		}
	}
	if noPatLines[0].Decision != DecisionDenyNoSession {
		t.Fatalf("Decision = %q, want deny-no-session", noPatLines[0].Decision)
	}

	all := append(append([]AuditLine{}, lines...), noPatLines...)
	for _, l := range all {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if strings.Contains(string(b), testCookie) {
			t.Fatalf("audit line leaked the session: %s", b)
		}
	}
}

func TestUpgradeRequestGetsRewrite(t *testing.T) {
	hub := newRecordingHub(t)
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(),
		ServeHost: "mac.ts.net"})
	req := proxyRequest("GET", "/events/ws", nil)
	req.Header.Set("Origin", "https://mac.ts.net")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	// The fake hub does not complete the upgrade; the headers show Rewrite ran.
	if n := hub.hits(); n != 1 {
		t.Fatalf("hub hit count = %d, want 1", n)
	}
	if c := hub.lastHeader().Get("Cookie"); c != sessionCookieName+"="+testCookie {
		t.Fatalf("Cookie = %q, want the injected session", c)
	}
	if a := hub.lastHeader().Get("Authorization"); a != "" {
		t.Fatalf("Authorization = %q, want none", a)
	}
}

// TestStripsSetCookieFromHubResponse: the hub's fresh scion_sess would be an
// unmanaged credential in the client.
func TestStripsSetCookieFromHubResponse(t *testing.T) {
	hub := newRecordingHub(t)
	hub.answerWith(200, http.Header{"Set-Cookie": {"scion_sess=abc123; Path=/; HttpOnly"}})

	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(),
		ServeHost: "mac.ts.net"})
	req := proxyRequest("GET", "/", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != 200 {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if c := rw.Header().Get("Set-Cookie"); c != "" {
		t.Fatalf("Set-Cookie leaked to client: %q", c)
	}
}

// TestOriginNullWithEmptyServeHostDenied: "null" parses to an empty host and
// must not equal an empty ServeHost.
func TestOriginNullWithEmptyServeHostDenied(t *testing.T) {
	assertGateRefuses(t, "", http.Header{"Origin": {"null"}})
}

func TestEmptyServeHostDeniesAnyOriginBearingRequest(t *testing.T) {
	origins := []string{"null", "foo", "/evil", "https://evil.example", "https://mac.ts.net"}
	for _, o := range origins {
		t.Run(o, func(t *testing.T) {
			assertGateRefuses(t, "", http.Header{"Origin": {o}})
		})
	}
}

func TestEmptyServeHostDeniesHeaderFreeRequestToo(t *testing.T) {
	assertGateRefuses(t, "", nil) // no Origin, no Sec-Fetch-Site
}

// TestInboundTailscaleHeadersStrippedFromForward: the gate trusts
// Tailscale-User-Login, but forwarding Tailscale-* would let a caller forge
// an identity the hub trusts.
func TestInboundTailscaleHeadersStrippedFromForward(t *testing.T) {
	hub := newRecordingHub(t)
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(),
		ServeHost: "mac.ts.net"})
	req := proxyRequest("GET", "/api/v1/agents", nil)
	req.Header.Set("Tailscale-User-Login", "admin@example.com")
	req.Header.Set("Tailscale-Foo", "bar")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 200 {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if v := hub.lastHeader().Get("Tailscale-User-Login"); v != "" {
		t.Fatalf("Tailscale-User-Login leaked to hub: %q", v)
	}
	if v := hub.lastHeader().Get("Tailscale-Foo"); v != "" {
		t.Fatalf("Tailscale-Foo leaked to hub: %q", v)
	}
}

func TestSubdomainOriginRejected(t *testing.T) {
	assertGateRefuses(t, "mac.ts.net", http.Header{"Origin": {"https://evil.mac.ts.net"}})
}

func TestSuffixOriginRejected(t *testing.T) {
	assertGateRefuses(t, "mac.ts.net", http.Header{"Origin": {"https://mac.ts.net.evil.example"}})
}

func TestServeHostCaseInsensitiveMatch(t *testing.T) {
	assertGateForwards(t, "mac.ts.net", http.Header{"Origin": {"https://MAC.TS.NET"}})
}

func TestMultiValueAndLowercaseSetCookieStripped(t *testing.T) {
	hub := newRecordingHub(t)
	hub.answerWith(200, http.Header{"Set-Cookie": {"scion_sess=abc123; Path=/", "other=xyz; Path=/"}})

	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(),
		ServeHost: "mac.ts.net"})
	req := proxyRequest("GET", "/", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != 200 {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if got := rw.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Fatalf("Set-Cookie leaked to client: %v", got)
	}
}

// TestMultipleOriginHeadersRejected: Header.Get reads only the first value,
// so a downstream reader could see a smuggled second one.
func TestMultipleOriginHeadersRejected(t *testing.T) {
	assertGateRefuses(t, "mac.ts.net", http.Header{"Origin": {"https://mac.ts.net", "https://evil.example"}})
}

func TestMultipleSecFetchSiteHeadersRejected(t *testing.T) {
	assertGateRefuses(t, "mac.ts.net", http.Header{"Sec-Fetch-Site": {"same-origin", "cross-site"}})
}

// TestSecFetchSiteAllowlist: same-site is a sibling tailnet name, and
// unknown values fail closed.
func TestSecFetchSiteAllowlist(t *testing.T) {
	cases := []struct {
		value      string
		wantStatus int
	}{
		{"same-origin", 200},
		{"same-site", http.StatusForbidden},
		{"none", 200},
		{"cross-site", http.StatusForbidden},
		{"unrecognized-value", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			hub := newRecordingHub(t)
			h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(),
				ServeHost: "mac.ts.net"})
			req := proxyRequest("GET", "/api/v1/agents", nil)
			req.Header.Set("Sec-Fetch-Site", tc.value)
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, req)
			if rw.Code != tc.wantStatus {
				t.Fatalf("Sec-Fetch-Site %q: status = %d, want %d", tc.value, rw.Code, tc.wantStatus)
			}
			wantHits := 0
			if tc.wantStatus == 200 {
				wantHits = 1
			}
			if n := hub.hits(); n != wantHits {
				t.Fatalf("Sec-Fetch-Site %q: hub hit count = %d, want %d", tc.value, n, wantHits)
			}
		})
	}
}

func TestAuditLineCapturesUpstreamStatusOnAllow(t *testing.T) {
	hub := newRecordingHub(t)
	hub.answerWith(201, nil)

	var lines []AuditLine
	h := NewHandler(Config{
		Target:    mustURL(t, hub.URL),
		Session:   testSession(),
		ServeHost: "mac.ts.net",
		Audit:     func(line AuditLine) { lines = append(lines, line) },
	})
	req := proxyRequest("GET", "/api/v1/agents", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != 201 {
		t.Fatalf("status = %d, want 201", rw.Code)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want 1", len(lines))
	}
	if lines[0].Decision != "allow" {
		t.Fatalf("Decision = %q, want allow", lines[0].Decision)
	}
	if lines[0].Status != 201 {
		t.Fatalf("Status = %d, want 201 (the real upstream status)", lines[0].Status)
	}
}

// TestAuditLineEmittedOnUpstreamFailure: a failed round trip bypasses
// ModifyResponse but is still audited once.
func TestAuditLineEmittedOnUpstreamFailure(t *testing.T) {
	unreachable := mustURL(t, "http://127.0.0.1:1")
	var lines []AuditLine
	h := NewHandler(Config{
		Target:    unreachable,
		Session:   testSession(),
		ServeHost: "mac.ts.net",
		Audit:     func(line AuditLine) { lines = append(lines, line) },
	})
	req := proxyRequest("GET", "/api/v1/agents", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want 1", len(lines))
	}
	if lines[0].Decision != "allow" {
		t.Fatalf("Decision = %q, want allow (the gate passed; the backend failed)", lines[0].Decision)
	}
	if lines[0].Status != http.StatusBadGateway {
		t.Fatalf("Status = %d, want %d", lines[0].Status, http.StatusBadGateway)
	}
}

// TestCustomDialContextIsUsed: the target is port 1, so a Transport still
// using the default dialer fails instead of reaching a local 8080 hub.
func TestCustomDialContextIsUsed(t *testing.T) {
	hub := newRecordingHub(t)
	hubAddr := mustURL(t, hub.URL).Host

	var dialed []string
	var mu sync.Mutex
	h := NewHandler(Config{
		Target:    mustURL(t, "http://127.0.0.1:1"),
		Session:   testSession(),
		ServeHost: "mac.ts.net",
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			dialed = append(dialed, addr)
			mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, network, hubAddr)
		},
	})
	req := proxyRequest("GET", "/api/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer attacker")
	req.Header.Set("Cookie", "scion_sess=evil")
	req.Header.Set("Tailscale-User-Login", "spoofed@example.com")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != 200 {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	if n := hub.hits(); n != 1 {
		t.Fatalf("hub hit count = %d, want 1", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) != 1 || dialed[0] != "127.0.0.1:1" {
		t.Fatalf("DialContext saw %v, want one dial of the Target address", dialed)
	}
	if a := hub.lastHeader().Get("Authorization"); a != "" {
		t.Fatalf("Authorization = %q leaked over the custom transport", a)
	}
	if c := hub.lastHeader().Get("Cookie"); c != sessionCookieName+"="+testCookie {
		t.Fatalf("Cookie = %q over the custom transport, want only the injected session", c)
	}
	if v := hub.lastHeader().Get("Tailscale-User-Login"); v != "" {
		t.Fatalf("Tailscale-User-Login leaked over the custom transport: %q", v)
	}
}

func TestTargetHostHeaderIsThePreservedHubAddress(t *testing.T) {
	var gotHost string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.WriteHeader(200)
	}))
	t.Cleanup(hub.Close)
	hubAddr := mustURL(t, hub.URL).Host

	h := NewHandler(Config{
		Target:    mustURL(t, "http://127.0.0.1:8080"),
		Session:   testSession(),
		ServeHost: "mac.ts.net",
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, hubAddr)
		},
	})
	h.ServeHTTP(httptest.NewRecorder(), proxyRequest("GET", "/healthz", nil))
	if gotHost != "127.0.0.1:8080" {
		t.Fatalf("Host = %q, want the guest-side hub address", gotHost)
	}
}

// TestDialFailureIsAudited: a 502 without a cause is indistinguishable from
// an auth failure or a wedged hub.
func TestDialFailureIsAudited(t *testing.T) {
	var lines []AuditLine
	h := NewHandler(Config{
		Target:    mustURL(t, "http://127.0.0.1:8080"),
		Session:   testSession(),
		ServeHost: "mac.ts.net",
		Audit:     func(line AuditLine) { lines = append(lines, line) },
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("nc is missing from the jail")
		},
	})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest("GET", "/healthz", nil))

	if rw.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rw.Code)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want 1", len(lines))
	}
	if !strings.Contains(lines[0].Error, "nc is missing from the jail") {
		t.Fatalf("audit line Error = %q, want the dialer's own diagnosis", lines[0].Error)
	}
	if strings.Contains(rw.Body.String(), "nc is missing") {
		t.Fatal("the transport's internals must not be echoed to the remote client")
	}
}

// TestSlowHubHeadersAreBounded: Serve has no write timeout, so only the
// header timeout stops a wedged jail holding the request forever.
func TestSlowHubHeadersAreBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	t.Cleanup(func() {
		close(accepted)
		for c := range accepted {
			c.Close()
		}
	})

	var lines []AuditLine
	h := NewHandler(Config{
		Target:    mustURL(t, "http://127.0.0.1:1"),
		Session:   testSession(),
		ServeHost: "mac.ts.net",
		Audit:     func(line AuditLine) { lines = append(lines, line) },
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
		},
	})

	setResponseHeaderTimeout(t, h, 150*time.Millisecond)

	rw := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rw, proxyRequest("GET", "/healthz", nil))
	elapsed := time.Since(start)

	if rw.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rw.Code)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("request took %v — the header wait is not bounded", elapsed)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want 1", len(lines))
	}
	if lines[0].Error == "" {
		t.Fatal("the audit line must record why the hub never answered")
	}
}

// TestStreamedBodyOutlivesTheHeaderTimeout: the bound is on headers only;
// attach streams run as long as the operator watches.
func TestStreamedBodyOutlivesTheHeaderTimeout(t *testing.T) {
	const timeout = 100 * time.Millisecond
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		rc := http.NewResponseController(w)
		for i := range 4 {
			fmt.Fprintf(w, "chunk%d\n", i)
			_ = rc.Flush()
			time.Sleep(timeout) // each gap alone exceeds the header timeout
		}
	}))
	defer hub.Close()
	hubAddr := mustURL(t, hub.URL).Host

	h := NewHandler(Config{
		Target:    mustURL(t, "http://127.0.0.1:1"),
		Session:   testSession(),
		ServeHost: "mac.ts.net",
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, hubAddr)
		},
	})
	setResponseHeaderTimeout(t, h, timeout)
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	// A real listener's Host is loopback; set the tailnet name.
	req, err := http.NewRequest(http.MethodGet, proxy.URL+"/api/v1/agents/x/transcript", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testServeHost
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read streamed body: %v", err)
	}
	if got := string(body); got != "chunk0\nchunk1\nchunk2\nchunk3\n" {
		t.Fatalf("streamed body = %q — the header timeout must not cut the body", got)
	}
}

// testServeHost is the tailnet name, which `tailscale serve` forwards as
// Host unchanged.
const testServeHost = "mac.ts.net"

// proxyRequest sets Host to testServeHost: httptest's "example.com" is
// refused.
func proxyRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Host = testServeHost
	return r
}

// TestHostGateRefusesDNSRebinding: with no Origin or Sec-Fetch-Site, only
// the Host check stops a rebound page; its Host is always the attacker's
// name.
func TestHostGateRefusesDNSRebinding(t *testing.T) {
	hub := newRecordingHub(t)
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(),
		ServeHost: testServeHost, ListenPort: 8445})

	for _, tc := range []struct {
		name, host string
		want       int
	}{
		{"the tailnet name", testServeHost, 200},
		{"loopback probe on the proxy's own port", "127.0.0.1:8445", 200},
		{"ipv6 loopback probe", "[::1]:8445", 200},
		{"localhost probe", "localhost:8445", 200},
		{"a rebound attacker domain", "evil.example", 403},
		{"a rebound attacker domain on our port", "evil.example:8445", 403},
		{"loopback on a DIFFERENT port", "127.0.0.1:9999", 403},
		{"the tailnet name as a suffix", "evil-mac.ts.net", 403},
		{"the tailnet name as a prefix", "mac.ts.net.evil.test", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := hub.hits()
			req := httptest.NewRequest("GET", "/api/v1/agents", nil)
			req.Host = tc.host
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			req.Header.Set("Tailscale-User-Login", "victim@example.com")
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, req)
			if rw.Code != tc.want {
				t.Fatalf("Host %q → %d, want %d", tc.host, rw.Code, tc.want)
			}
			if tc.want == 403 && hub.hits() != before {
				t.Error("a refused request still reached the hub")
			}
		})
	}
}

// TestAuditFieldsAreBoundedOnEveryDecisionPath: denials are audited before
// any identity check, so caller text must be capped.
func TestAuditFieldsAreBoundedOnEveryDecisionPath(t *testing.T) {
	huge := strings.Repeat("A", 4096)
	for _, tc := range []struct {
		name     string
		host     string
		allowed  []string
		session  *stubSession
		decision Decision
	}{
		{"denied before any identity check", "evil.example", nil, testSession(), "deny-host"},
		{"denied by the allowlist", testServeHost, []string{"a@example.com"}, testSession(), "deny-user"},
		{"denied for a failed login", testServeHost, nil, &stubSession{err: errors.New("down")}, "deny-no-session"},
		{"allowed", testServeHost, nil, testSession(), "allow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newRecordingHub(t)
			sink := &auditSink{}
			h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: tc.session,
				ServeHost: testServeHost, ListenPort: 8445, AllowedUsers: tc.allowed, Audit: sink.add})
			req := httptest.NewRequest(huge, "/"+huge, nil)
			req.Host = tc.host
			req.Header.Set("Tailscale-User-Login", huge+"@example.com")
			h.ServeHTTP(httptest.NewRecorder(), req)

			lines := sink.all()
			if len(lines) != 1 {
				t.Fatalf("%d audit lines, want exactly 1", len(lines))
			}
			l := lines[0]
			if l.Decision != tc.decision {
				t.Fatalf("decision = %q, want %q", l.Decision, tc.decision)
			}
			for _, f := range []struct{ name, value string }{
				{"path", l.Path}, {"method", l.Method}, {"ts_login", l.TSLogin},
			} {
				if len(f.value) > maxAuditFieldLen+len("…") {
					t.Errorf("audited %s is %d bytes, want at most %d", f.name, len(f.value), maxAuditFieldLen+len("…"))
				}
				if strings.Contains(f.value, huge) {
					t.Errorf("audited %s carries the caller's text whole", f.name)
				}
			}
		})
	}
}

// TestTheGateDecidesOnTheWholeLoginNotTheAuditCopy: deciding on the
// truncated copy would merge logins sharing a prefix.
func TestTheGateDecidesOnTheWholeLoginNotTheAuditCopy(t *testing.T) {
	prefix := strings.Repeat("a", maxAuditFieldLen)
	allowed := prefix + "-real@example.com"
	twin := prefix + "-impostor@example.com"

	hub := newRecordingHub(t)
	sess := &stubSession{cookie: "sess-1"}
	sink := &auditSink{}
	h := NewHandler(Config{Target: mustURL(t, hub.URL),
		ServeHost: testServeHost, AllowedUsers: []string{allowed}, Session: sess, Audit: sink.add})

	req := proxyRequest("GET", "/", nil)
	req.Header.Set("Tailscale-User-Login", allowed)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 200 {
		t.Fatalf("the operator on the allowlist was refused: %d", rw.Code)
	}

	req = proxyRequest("GET", "/", nil)
	req.Header.Set("Tailscale-User-Login", twin)
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Fatalf("a login sharing the allowed one's first %d bytes was admitted: %d", maxAuditFieldLen, rw.Code)
	}
	if n := hub.hits(); n != 1 {
		t.Fatalf("the hub was reached %d times, want 1 (only the allowed operator)", n)
	}

	if _, _, logins := sess.state(); len(logins) != 1 || logins[0] != allowed {
		t.Fatalf("the hub login was performed for %q, want the untruncated %q", logins, allowed)
	}
	for _, l := range sink.all() {
		if len(l.TSLogin) > maxAuditFieldLen+len("…") {
			t.Fatalf("audited ts_login is %d bytes, want at most %d", len(l.TSLogin), maxAuditFieldLen+len("…"))
		}
	}
}

// setResponseHeaderTimeout lowers the header wait for tests; production has
// no reason to, so it is not a Config field.
func setResponseHeaderTimeout(t *testing.T, h http.Handler, d time.Duration) {
	t.Helper()
	tr, ok := h.(*gate).rp.Transport.(*http.Transport)
	if !ok {
		t.Fatal("handler has no jail transport — the config needs DialContext")
	}
	tr.ResponseHeaderTimeout = d
}

// TestHubRoutesTheProxyNeverForwards: the rows cover every /api/v1/system
// route in scion's pkg/hub/route_metadata.go.
func TestHubRoutesTheProxyNeverForwards(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		refused      bool
	}{
		{"GET", "/api/v1/system/status", false},
		{"HEAD", "/api/v1/system/status", false},
		{"POST", "/api/v1/system/status", true},
		{"PUT", "/api/v1/system/status", true},
		{"GET", "/api/v1/system/status/", true},
		{"GET", "/api/v1/system/identity", true},
		{"PUT", "/api/v1/system/identity", true},
		{"GET", "/api/v1/system/check", true},
		{"GET", "/api/v1/system/runtime", true},
		{"PUT", "/api/v1/system/runtime", true},
		{"POST", "/api/v1/system/init", true},
		{"POST", "/api/v1/system/images/pull", true},
		{"POST", "/api/v1/system/images/build", true},
		{"GET", "/api/v1/system/apple-dns", true},
		{"POST", "/api/v1/system/apple-dns", true},
		{"GET", "/api/v1/system/registry", true},
		{"PUT", "/api/v1/system/registry", true},
		{"GET", "/api/v1/system/workstation-settings", true},
		{"PUT", "/api/v1/system/workstation-settings", true},
		{"GET", "/api/v1/system/fs/list", true},
		{"POST", "/api/v1/system/fs/mkdir", true},
		{"GET", "/api/v1/system/fs/validate-path", true},
		{"GET", "/api/v1/system/some-future-route", true},
		{"GET", "/api/v1/system", true},
		{"PUT", "/api/v1/%73ystem/registry", true},
		{"PUT", "/api/v1/system/%72egistry", true},

		{"GET", "/api/v1/projects", false},
		{"HEAD", "/api/v1/projects", false},
		{"POST", "/api/v1/projects", true},
		{"PUT", "/api/v1/projects", true},
		{"POST", "/api/v1/%70rojects", true},
		{"POST", "/api/v1/projects/register", true},
		{"GET", "/api/v1/projects/register", true},
		{"POST", "/api/v1/projects/register/", true},
		{"POST", "/api/v1/projects/p1/clone", true},
		{"POST", "/api/v1/projects/uuid__slug/clone", true},
		{"POST", "/api/v1/projects/p1/%63lone", true},
		{"POST", "/api/v1/projects/p1/clone/", true},
		{"GET", "/api/v1/groves", false},
		{"POST", "/api/v1/groves", true},
		{"POST", "/api/v1/groves/register", true},
		{"POST", "/api/v1/groves/p1/clone", true},

		{"GET", "/api/v1/projects/p1", false},
		{"GET", "/api/v1/projects/p1/agents", false},
		{"POST", "/api/v1/projects/p1/agents", false},
		{"POST", "/api/v1/projects/p1/agents/a1/messages", false},
		{"GET", "/api/v1/projects/cloned-thing/settings", false},
		{"GET", "/api/v1/groves/p1", false},
		{"GET", "/api/v1/systems", false},

		{"GET", "/api/v1/conduit", true},
		{"GET", "/api/v1/conduit/grant-keys", true},
		{"POST", "/api/v1/admin/conduit/grant-keys/rotate", true},
		{"GET", "/api/v1/artifacts", true},
		{"POST", "/api/v1/artifacts", true},
		{"GET", "/api/v1/artifacts/a1", true},
		{"GET", "/api/v1/artifacts/shared/tok", true},
		{"GET", "/api/v1/artifacts/view/cap/index.html", true},
		{"PUT", "/api/v1/admin/experiments", true},
		{"POST", "/api/v1/admin/experiments", true},
		{"DELETE", "/api/v1/admin/experiments", true},
		{"PATCH", "/api/v1/admin/experiments", true},
		{"GET", "/api/v1/admin/experiments", false},
		{"HEAD", "/api/v1/admin/experiments", false},
		{"GET", "/api/v1/experiments", false},
		{"GET", "/api/v1/agents/a1/ports/tunnel", true},
		{"POST", "/api/v1/agents/a1/delegations", true},
		{"GET", "/api/v1/agents/a1/delegations", true},
		{"POST", "/api/v1/agents/a1/delegations/g1/exchange", true},
		{"GET", "/api/v1/agents/a1/ports", false},
		{"GET", "/api/v1/agents/a1/pty", false},
		{"GET", "/api/v1/users/me/delegations", false},
		{"POST", "/api/v1/test-identities", true},
		{"POST", "/api/v1/test-identities/t1/web-login-code", true},
		{"POST", "/api/v1/test-identity-delegations", true},
		{"POST", "/auth/test-identity/redeem", true},
		{"GET", "/api/v1/test-infra/status", false},
		{"POST", "/api/v1/brokers", true},
		{"POST", "/api/v1/brokers/join", true},
		{"DELETE", "/api/v1/brokers/b1", true},
		{"PUT", "/api/v1/runtime-brokers/b1", true},
		{"DELETE", "/api/v1/runtime-brokers/b1", true},
		{"POST", "/api/v1/runtime-brokers/b1/heartbeat", true},
		{"GET", "/api/v1/runtime-brokers", false},
		{"GET", "/api/v1/runtime-brokers/b1", false},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			hub := newRecordingHub(t)
			var lines []AuditLine
			h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(),
				ServeHost: "mac.ts.net", Audit: func(l AuditLine) { lines = append(lines, l) }})
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, proxyRequest(tc.method, tc.path, nil))
			if tc.refused {
				if rw.Code != http.StatusForbidden || hub.hits() != 0 || len(lines) != 1 || lines[0].Decision != DecisionDenyRoute {
					t.Fatalf("status %d, hub hits %d, audit %+v; want 403, 0, deny-route", rw.Code, hub.hits(), lines)
				}
				return
			}
			if rw.Code != 200 || hub.hits() != 1 {
				t.Fatalf("status %d, hub hits %d; want it forwarded", rw.Code, hub.hits())
			}
		})
	}
}

// TestFencedRouteSpellings: no spelling of a fenced route reaches the hub,
// whichever check refuses it.
func TestFencedRouteSpellings(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/v1/conduit/"},
		{"GET", "/api/v1/conduit."},
		{"GET", "/api/v1/conduit;x"},
		{"GET", "/api/v1/conduit%20"},
		{"GET", "/api/v1/Conduit"},
		{"GET", "/API/V1/CONDUIT"},
		{"GET", "/api/v1/%63onduit"},
		{"GET", "/api/v1/%43onduit"},
		{"GET", "/api/v1//conduit"},
		{"GET", "/api/v1/./conduit"},
		{"GET", "/api/v1/x/../conduit"},
		{"GET", "/api/v1/x/%2e%2e/conduit"},
		{"GET", "/api/v1%2fconduit"},
		{"GET", "/api/v1/conduit/grant-keys/"},
		{"GET", "/api/v1/conduit/Grant-Keys"},
		{"POST", "/api/v1/admin/conduit/grant-keys/rotate/"},
		{"POST", "/api/v1/Admin/Conduit/grant-keys/rotate"},
		{"GET", "/api/v1/artifacts/"},
		{"GET", "/api/v1/Artifacts/shared/tok"},
		{"GET", "/api/v1/artifacts/%73hared/tok"},
		{"GET", "/api/v1/artifacts.json"},
		{"GET", "/api/v1/artifacts/view/cap/../../a1"},
		{"PUT", "/api/v1/admin/experiments/"},
		{"PUT", "/api/v1/admin/Experiments"},
		{"PUT", "/api/v1/admin/%65xperiments"},
		{"DELETE", "/api/v1/admin/experiments."},
		{"put", "/api/v1/admin/experiments"},
		{"GET", "/api/v1/agents/a1/ports/tunnel/"},
		{"GET", "/api/v1/agents/a1/Ports/Tunnel"},
		{"GET", "/api/v1/agents/a1/ports/%74unnel"},
		{"POST", "/api/v1/agents/a1/Delegations"},
		{"POST", "/Auth/Test-Identity/redeem"},
		{"POST", "/auth/test-identity/redeem/"},
		{"POST", "/api/v1/Test-Identities"},
		{"POST", "/api/v1/Brokers"},
		{"POST", "/api/v1/brokers/"},
		{"POST", "/api/v1/brokers."},
		{"POST", "/api/v1/%62rokers/join"},
		{"POST", "/api/v1/brokers/Join"},
		{"PATCH", "/api/v1/Runtime-Brokers/b1"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			hub := newRecordingHub(t)
			var lines []AuditLine
			h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(),
				ServeHost: "mac.ts.net", Audit: func(l AuditLine) { lines = append(lines, l) }})
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, proxyRequest(tc.method, tc.path, nil))
			if hub.hits() != 0 || rw.Code < 400 || rw.Code > 499 || len(lines) != 1 ||
				(lines[0].Decision != DecisionDenyRoute && lines[0].Decision != DecisionDenyPath) {
				t.Fatalf("status %d, hub hits %d, audit %+v; want refused before the hub", rw.Code, hub.hits(), lines)
			}
		})
	}
}

// TestClientIdentityHeadersStrippedFromForward: every header scion's auth
// middleware reads as identity is the proxy's to set or no one's.
func TestClientIdentityHeadersStrippedFromForward(t *testing.T) {
	hub := newRecordingHub(t)
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(), ServeHost: "mac.ts.net"})
	stripped := []string{
		"X-Scion-Agent-Token", "X-Scion-Broker-ID", "X-Scion-Broker-Token", "X-Scion-Signature",
		"X-Scion-Timestamp", "X-Scion-Nonce", "X-Scion-Signed-Headers", "X-Scion-On-Behalf-Of",
		"X-Scion-Federation-Token", "X-Scion-Plugin-Name",
		"X-Forwarded-User-Id", "X-Forwarded-User-Email", "X-Forwarded-User-Name", "X-Forwarded-User-Role",
		"X-Goog-IAP-JWT-Assertion", "X-API-Key", "X-Lever-Hub-Assertion",
	}
	req := proxyRequest("GET", "/api/v1/agents", nil)
	for _, k := range stripped {
		req.Header.Set(k, "forged")
	}
	req.Header.Set("x-scion-agent-token", "forged-lowercase")
	// A non-canonical key bypasses Header.Set's canonicalization.
	req.Header["x-lever-hub-assertion"] = []string{"forged-lowercase"}
	req.Header.Set("X-Preview-Token", "kept")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 200 {
		t.Fatalf("status = %d, want 200", rw.Code)
	}
	got := hub.lastHeader()
	for _, k := range stripped {
		if v := got.Values(k); len(v) != 0 {
			t.Errorf("%s reached the hub: %q", k, v)
		}
	}
	if got.Get("X-Preview-Token") != "kept" {
		t.Errorf("X-Preview-Token was stripped; it names no identity")
	}
}

func TestClientIdentityHeader(t *testing.T) {
	for k, want := range map[string]bool{
		"Tailscale-User-Login": true, "X-Scion-Agent-Token": true, "x-scion-anything": true,
		"X-Forwarded-User-Email": true, "X-Goog-Iap-Jwt-Assertion": true, "X-Api-Key": true,
		"X-Lever-Hub-Assertion": true, "x-lever-hub-assertion": true, "X-LEVER-HUB-ASSERTION": true,
		"X-Forwarded-For": false, "X-Forwarded-Proto": false, "Accept": false, "X-Preview-Token": false,
	} {
		if got := clientIdentityHeader(k); got != want {
			t.Errorf("clientIdentityHeader(%q) = %v, want %v", k, got, want)
		}
	}
}

// navRequest is a cross-site top-level page load with no Origin. In hdr, an
// empty value deletes a header and a repeated key sends every value.
func navRequest(method, target, login string, hdr ...string) *http.Request {
	req := proxyRequest(method, target, nil)
	if login != "" {
		req.Header.Set("Tailscale-User-Login", login)
	}
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	set := map[string]bool{}
	for i := 0; i+1 < len(hdr); i += 2 {
		k, v := http.CanonicalHeaderKey(hdr[i]), hdr[i+1]
		if !set[k] {
			req.Header.Del(k)
			set[k] = true
		}
		if v != "" {
			req.Header.Add(k, v)
		}
	}
	return req
}

// TestEntryNavigation: only a top-level GET/HEAD navigation to an entry page
// with no Origin passes cross-site; another path, method, frame, fetch,
// WebSocket, missing or repeated Fetch Metadata, or an Origin is refused.
func TestEntryNavigation(t *testing.T) {
	cases := []struct {
		name, method, path string
		hdr                []string
		want               int
		reason             string
	}{
		{"cross-site GET /", "GET", "/", nil, http.StatusFound, ReasonCrossSiteNavigation},
		{"cross-site GET chat page", "GET", chatPagePath, nil, http.StatusOK, ReasonCrossSiteNavigation},
		{"same-site GET /", "GET", "/", []string{"Sec-Fetch-Site", "same-site"}, http.StatusFound, ReasonSameSiteNavigation},
		{"cross-site HEAD /", "HEAD", "/", nil, http.StatusFound, ReasonCrossSiteNavigation},
		{"cross-site HEAD chat page", "HEAD", chatPagePath, nil, http.StatusOK, ReasonCrossSiteNavigation},
		{"query on /", "GET", "/?from=mail", nil, http.StatusFound, ReasonCrossSiteNavigation},
		{"user activation", "GET", "/", []string{"Sec-Fetch-User", "?1"}, http.StatusFound, ReasonCrossSiteNavigation},
		{"upper-case values", "GET", "/", []string{"Sec-Fetch-Mode", "Navigate", "Sec-Fetch-Dest", "Document"}, http.StatusFound, ReasonCrossSiteNavigation},
		{"same-origin keeps no reason", "GET", "/", []string{"Sec-Fetch-Site", "same-origin"}, http.StatusFound, ""},

		{"lever API", "GET", chatAgentsPath, nil, http.StatusForbidden, ""},
		{"operator view", "GET", chatContactsPath, nil, http.StatusForbidden, ""},
		{"push key", "GET", "/lever/api/push/key", nil, http.StatusForbidden, ""},
		{"chat script", "GET", "/lever/chat.js", nil, http.StatusForbidden, ""},
		{"manifest", "GET", chatManifestPath, nil, http.StatusForbidden, ""},
		{"lever prefix", "GET", "/lever", nil, http.StatusForbidden, ""},
		{"chat page with a slash", "GET", chatPagePath + "/", nil, http.StatusForbidden, ""},
		{"hub API", "GET", "/api/v1/agents", nil, http.StatusForbidden, ""},
		{"hub page", "GET", "/agents", nil, http.StatusForbidden, ""},
		{"hub chat page", "GET", "/chat/dm/x", nil, http.StatusForbidden, ""},
		{"sign-in route", "GET", "/auth/login", nil, http.StatusForbidden, ""},
		{"POST navigation", "POST", "/", nil, http.StatusForbidden, ""},
		{"PUT", "PUT", "/", nil, http.StatusForbidden, ""},
		{"DELETE", "DELETE", chatPagePath, nil, http.StatusForbidden, ""},
		{"iframe", "GET", "/", []string{"Sec-Fetch-Dest", "iframe"}, http.StatusForbidden, ""},
		{"frame", "GET", "/", []string{"Sec-Fetch-Dest", "frame"}, http.StatusForbidden, ""},
		{"embed", "GET", "/", []string{"Sec-Fetch-Dest", "embed", "Sec-Fetch-Mode", "no-cors"}, http.StatusForbidden, ""},
		{"object", "GET", "/", []string{"Sec-Fetch-Dest", "object"}, http.StatusForbidden, ""},
		{"fetch", "GET", "/", []string{"Sec-Fetch-Mode", "cors", "Sec-Fetch-Dest", "empty"}, http.StatusForbidden, ""},
		{"subresource", "GET", chatPagePath, []string{"Sec-Fetch-Mode", "no-cors", "Sec-Fetch-Dest", "script"}, http.StatusForbidden, ""},
		{"WebSocket", "GET", "/", []string{"Sec-Fetch-Mode", "websocket", "Sec-Fetch-Dest", "empty", "Upgrade", "websocket", "Connection", "Upgrade"}, http.StatusForbidden, ""},
		{"no Sec-Fetch-Mode", "GET", "/", []string{"Sec-Fetch-Mode", ""}, http.StatusForbidden, ""},
		{"no Sec-Fetch-Dest", "GET", "/", []string{"Sec-Fetch-Dest", ""}, http.StatusForbidden, ""},
		{"two Sec-Fetch-Mode", "GET", "/", []string{"Sec-Fetch-Mode", "navigate", "Sec-Fetch-Mode", "navigate"}, http.StatusForbidden, ""},
		{"two Sec-Fetch-Dest", "GET", "/", []string{"Sec-Fetch-Dest", "document", "Sec-Fetch-Dest", "iframe"}, http.StatusForbidden, ""},
		{"two Sec-Fetch-Site", "GET", "/", []string{"Sec-Fetch-Site", "cross-site", "Sec-Fetch-Site", "cross-site"}, http.StatusForbidden, ""},
		{"unknown Sec-Fetch-Site", "GET", "/", []string{"Sec-Fetch-Site", "cross-origin"}, http.StatusForbidden, ""},
		{"Origin of another site", "GET", "/", []string{"Origin", "https://evil.test"}, http.StatusForbidden, ""},
		{"Origin null", "GET", "/", []string{"Origin", "null"}, http.StatusForbidden, ""},
		{"own Origin on a cross-site load", "GET", "/", []string{"Origin", "https://" + testServeHost}, http.StatusForbidden, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newPageHub(t)
			var lines []AuditLine
			cfg := chatConfig(t, hub)
			cfg.Audit = func(l AuditLine) { lines = append(lines, l) }
			rw := httptest.NewRecorder()
			NewHandler(cfg).ServeHTTP(rw, navRequest(tc.method, tc.path, chatOp, tc.hdr...))
			if rw.Code != tc.want {
				t.Fatalf("status %d, want %d (%s)", rw.Code, tc.want, rw.Body)
			}
			if len(lines) != 1 {
				t.Fatalf("audit lines = %d, want 1", len(lines))
			}
			if lines[0].Reason != tc.reason {
				t.Errorf("audit reason = %q, want %q", lines[0].Reason, tc.reason)
			}
			if tc.want == http.StatusForbidden {
				if lines[0].Decision != DecisionDenyOrigin {
					t.Errorf("decision = %q, want %q", lines[0].Decision, DecisionDenyOrigin)
				}
				if got := hub.reached(); len(got) != 0 {
					t.Errorf("a refused navigation reached the hub: %v", got)
				}
				return
			}
			if lines[0].Decision != DecisionAllow {
				t.Errorf("decision = %q, want allow", lines[0].Decision)
			}
			if !strings.Contains(rw.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || rw.Header().Get("X-Frame-Options") != "DENY" {
				t.Errorf("an entry page may be framed: %v", rw.Header())
			}
		})
	}
}

// TestEntryNavigationToTheHub: with the chat page off, "/" is still an entry
// page and its answer forbids framing; /lever/chat is not one.
func TestEntryNavigationToTheHub(t *testing.T) {
	hub := newRecordingHub(t)
	var lines []AuditLine
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(), ServeHost: testServeHost,
		Audit: func(l AuditLine) { lines = append(lines, l) }})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, navRequest("GET", "/", ""))
	if rw.Code != http.StatusOK || hub.hits() != 1 {
		t.Fatalf("GET /: status %d, hub hits %d; want 200 and 1", rw.Code, hub.hits())
	}
	if len(lines) != 1 || lines[0].Decision != DecisionAllow || lines[0].Reason != ReasonCrossSiteNavigation {
		t.Errorf("audit = %+v, want allow with %q", lines, ReasonCrossSiteNavigation)
	}
	csp := rw.Header().Values("Content-Security-Policy")
	if len(csp) != 1 || csp[0] != "frame-ancestors 'none'" || rw.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("hub entry page framing headers: CSP %q, XFO %q", csp, rw.Header().Get("X-Frame-Options"))
	}
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, navRequest("GET", chatPagePath, ""))
	if rw.Code != http.StatusForbidden || hub.hits() != 1 {
		t.Errorf("GET %s with the chat page off: status %d, hub hits %d; want 403 and still 1", chatPagePath, rw.Code, hub.hits())
	}
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest("GET", "/agents", nil))
	if v := rw.Header().Values("Content-Security-Policy"); len(v) != 0 {
		t.Errorf("GET /agents: CSP %q added, want none", v)
	}
}

func TestEntryNavigationContactLanding(t *testing.T) {
	hub := newPageHub(t)
	cfg := chatConfig(t, hub)
	cfg.ChatAgent = ""
	rw := httptest.NewRecorder()
	NewHandler(cfg).ServeHTTP(rw, navRequest("GET", "/", "c@x"))
	if rw.Code != http.StatusOK || !strings.HasPrefix(rw.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("status %d, type %q; want the landing page", rw.Code, rw.Header().Get("Content-Type"))
	}
	if !strings.Contains(rw.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || rw.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("the contact landing page may be framed: %v", rw.Header())
	}
}

func TestRefusedNavigationIsAPage(t *testing.T) {
	hub := newPageHub(t)
	var lines []AuditLine
	cfg := chatConfig(t, hub)
	cfg.Audit = func(l AuditLine) { lines = append(lines, l) }
	h := NewHandler(cfg)

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, navRequest("GET", "/agents", chatOp))
	if rw.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rw.Code)
	}
	for k, want := range map[string]string{
		"Content-Type":            "text/html; charset=utf-8",
		"Cache-Control":           "no-store",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Content-Security-Policy": "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
	} {
		if got := rw.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	body := rw.Body.String()
	for _, want := range []string{"<p>cross-site request refused</p>", `<a href="/">Open the chat</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q lacks %q", body, want)
		}
	}
	if strings.Contains(strings.ToLower(body), "<script") {
		t.Errorf("refusal page has a script: %q", body)
	}
	if len(lines) != 1 || lines[0].Decision != DecisionDenyOrigin || lines[0].Status != http.StatusForbidden {
		t.Errorf("audit = %+v, want one deny-origin 403", lines)
	}

	for _, req := range []*http.Request{navRequest("POST", "/", chatOp), navRequest("GET", "/", chatOp, "Origin", "https://evil.test")} {
		rw = httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusForbidden || rw.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Errorf("%s %s Origin %q: %d %q, want a 403 page", req.Method, req.URL.Path, req.Header.Get("Origin"), rw.Code, rw.Header().Get("Content-Type"))
		}
	}

	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, navRequest("HEAD", "/agents", chatOp))
	if rw.Code != http.StatusForbidden || rw.Header().Get("Content-Type") != "text/html; charset=utf-8" || rw.Body.Len() != 0 {
		t.Errorf("HEAD: %d %q %d bytes, want a bodiless 403 page", rw.Code, rw.Header().Get("Content-Type"), rw.Body.Len())
	}

	for name, hdr := range map[string][]string{
		"iframe":          {"Sec-Fetch-Dest", "iframe"},
		"fetch":           {"Sec-Fetch-Mode", "cors", "Sec-Fetch-Dest", "empty"},
		"no fetch mode":   {"Sec-Fetch-Mode", ""},
		"two fetch modes": {"Sec-Fetch-Mode", "navigate", "Sec-Fetch-Mode", "navigate"},
	} {
		rw = httptest.NewRecorder()
		h.ServeHTTP(rw, navRequest("GET", "/agents", chatOp, hdr...))
		if rw.Code != http.StatusForbidden || !strings.HasPrefix(rw.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("%s: %d %q, want a text/plain 403", name, rw.Code, rw.Header().Get("Content-Type"))
		}
	}
}

func TestRefusalPageEscapes(t *testing.T) {
	got := refusalPage(`<b>"x"</b>`)
	if strings.Contains(got, "<b>") || !strings.Contains(got, "&lt;b&gt;&#34;x&#34;&lt;/b&gt;") {
		t.Errorf("refusalPage did not escape: %q", got)
	}
}

// TestRefusedNavigationCarriesNoReason: a navigation denied by a later
// check is audited without a navigation reason.
func TestRefusedNavigationCarriesNoReason(t *testing.T) {
	hub := newPageHub(t)
	var lines []AuditLine
	cfg := chatConfig(t, hub)
	cfg.Audit = func(l AuditLine) { lines = append(lines, l) }
	rw := httptest.NewRecorder()
	NewHandler(cfg).ServeHTTP(rw, navRequest("GET", "/", "stranger@x"))
	if rw.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rw.Code)
	}
	if len(lines) != 1 || lines[0].Decision != DecisionDenyUser || lines[0].Reason != "" {
		t.Fatalf("audit = %+v, want one deny-user line with no reason", lines)
	}
	if got := hub.reached(); len(got) != 0 {
		t.Errorf("the hub was reached: %v", got)
	}
}
