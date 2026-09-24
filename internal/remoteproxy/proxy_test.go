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

// TestPhoneCannotChooseItsIdentity: a phone-supplied Authorization header is
// stripped, not forwarded — scion's sessionToBearerMiddleware lets any request
// that carries one straight through, so forwarding it would let the phone pick
// the hub identity the API runs as. The phone's own Cookie is replaced by the
// operator's session, never merged with it.
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

// gateProbe serves one GET /api/v1/agents through a handler bound to
// serveHost, with hdr added to the request (a repeated key sends every value),
// and returns the response status and how many times the hub was reached.
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

// assertGateRefuses pins the deny shape of the origin gate: 403 and the hub
// never reached.
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

// assertGateForwards pins the allow shape: 200 and exactly one hub hit.
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

// TestNoSessionSourceFailsClosed: without a login driver the proxy has no
// identity to send, so it refuses rather than forward anonymous requests.
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

// TestCredentialMintRoutesRefused: the session the proxy injects may mint
// hub credentials (scion lets a session credential create a UAT), and one in
// a response body would leave the host. /api/v1/auth/ is allowlisted, so an
// unknown route there is refused too; the SPA's token page keeps listing and
// revoking.
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

	// One request per decision path.
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
	// Separate handler whose login fails, for the deny-no-session path.
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
	// The fake hub does not complete the upgrade (plain 200 response), so
	// the ReverseProxy treats it as a normal response and we can inspect
	// the headers it received to prove the Rewrite hook ran on this
	// upgrade-shaped request.
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

// TestStripsSetCookieFromHubResponse: the hub mints a fresh scion_sess cookie on every
// cookie-less request; the phone must never receive it, since a leaked
// scion_sess would be an alternate, lever-unmanaged credential.
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

// --- Fix round 1: adversarial review findings ---

// TestOriginNullWithEmptyServeHostDenied is the minimal repro of the
// fail-open bug: an unconfigured ServeHost ("") combined with an Origin
// that parses to an empty host (browsers send exactly this for sandboxed/
// opaque origins) made url.Parse's u.Host ("") case-insensitively equal
// cfg.ServeHost (""), so the request was WRONGLY allowed and forwarded with
// the credential injected. base_url is optional in config, so ServeHost=="" is a
// reachable misconfiguration, not a hypothetical.
func TestOriginNullWithEmptyServeHostDenied(t *testing.T) {
	assertGateRefuses(t, "", http.Header{"Origin": {"null"}})
}

// TestEmptyServeHostDeniesAnyOriginBearingRequest pins that an unconfigured
// ServeHost rejects every Origin-bearing request, regardless of what the
// Origin value parses to (empty host, non-empty host, malformed).
func TestEmptyServeHostDeniesAnyOriginBearingRequest(t *testing.T) {
	origins := []string{"null", "foo", "/evil", "https://evil.example", "https://mac.ts.net"}
	for _, o := range origins {
		t.Run(o, func(t *testing.T) {
			assertGateRefuses(t, "", http.Header{"Origin": {o}})
		})
	}
}

// TestEmptyServeHostDeniesHeaderFreeRequestToo pins the stricter half of the
// fix: an unconfigured ServeHost fails closed for EVERY request, not just
// Origin-bearing ones — a curl/native request with no Origin and no
// Sec-Fetch-Site header must also be refused, since "unconfigured" can
// never be distinguished from "misconfigured" from inside the handler.
func TestEmptyServeHostDeniesHeaderFreeRequestToo(t *testing.T) {
	assertGateRefuses(t, "", nil) // no Origin, no Sec-Fetch-Site
}

// TestInboundTailscaleHeadersStrippedFromForward: the handler trusts a
// client-supplied Tailscale-User-Login for its OWN AllowedUsers gate (that
// trust is a documented precondition on the loopback bind + tailscale
// serve, enforced by the caller — see the package doc comment), but must
// never forward client-supplied Tailscale-* headers to the hub, or a caller
// could forge an identity the HUB itself trusts.
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

// TestSubdomainOriginRejected pins the exact-match rule: a subdomain of
// ServeHost must not pass (guards against an accidental HasSuffix/Contains
// regression).
func TestSubdomainOriginRejected(t *testing.T) {
	assertGateRefuses(t, "mac.ts.net", http.Header{"Origin": {"https://evil.mac.ts.net"}})
}

// TestSuffixOriginRejected pins the exact-match rule from the other
// direction: ServeHost as a PREFIX of the Origin host must not pass.
func TestSuffixOriginRejected(t *testing.T) {
	assertGateRefuses(t, "mac.ts.net", http.Header{"Origin": {"https://mac.ts.net.evil.example"}})
}

// TestServeHostCaseInsensitiveMatch pins the intentional EqualFold
// case-insensitivity: an Origin host that differs from ServeHost only in
// case must still be accepted.
func TestServeHostCaseInsensitiveMatch(t *testing.T) {
	assertGateForwards(t, "mac.ts.net", http.Header{"Origin": {"https://MAC.TS.NET"}})
}

// TestMultiValueAndLowercaseSetCookieStripped: the hub may emit more than
// one Set-Cookie header, and http.Header canonicalizes key case on write
// (so "set-cookie" and "Set-Cookie" land under the same canonical key) —
// pin that Header.Del("Set-Cookie") removes ALL of them regardless.
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

// TestMultipleOriginHeadersRejected: a client sending two Origin header
// values (legal at the wire level) must not be able to have the gate
// evaluate one value while a downstream reader (or the hub) sees another —
// r.Header.Get only ever inspects the FIRST value, so a client could put an
// allowed host first and an attacker-controlled host second. Reject
// outright rather than pick a value.
func TestMultipleOriginHeadersRejected(t *testing.T) {
	assertGateRefuses(t, "mac.ts.net", http.Header{"Origin": {"https://mac.ts.net", "https://evil.example"}})
}

// TestMultipleSecFetchSiteHeadersRejected: same smuggling concern as
// multi-value Origin, for Sec-Fetch-Site.
func TestMultipleSecFetchSiteHeadersRejected(t *testing.T) {
	assertGateRefuses(t, "mac.ts.net", http.Header{"Sec-Fetch-Site": {"same-origin", "cross-site"}})
}

// TestSecFetchSiteAllowlist: Sec-Fetch-Site must be an allowlist of
// {same-origin, none} rather than a denylist of {cross-site} — same-site is
// a sibling tailnet name, not this proxy's origin, and
// any other/unrecognized value (a future spec addition, a spoofed value
// from a non-browser client, a typo) must be refused, not silently passed.
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

// TestAuditLineCapturesUpstreamStatusOnAllow: the allow-path AuditLine must
// carry the real upstream status code, not the zero value — the audit call
// used to fire before the round trip even started.
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

// TestAuditLineEmittedOnUpstreamFailure: the "exactly one AuditLine per
// request" contract must hold even when the round trip to the hub fails
// outright (hub down/unreachable) — that path bypasses ModifyResponse, so
// the audit call must not be silently dropped there.
func TestAuditLineEmittedOnUpstreamFailure(t *testing.T) {
	// A target with nothing listening: connection refused.
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

// TestCustomDialContextIsUsed proves cfg.DialContext actually carries the
// proxied request — the jail transport's whole point — and that session
// injection and header stripping still apply on that path. The target is
// deliberately an address nothing listens on (port 1, standing in for the
// real 127.0.0.1:8080): only a dialer that ignores it and reaches the test
// server can succeed, so a Transport that quietly kept the default dialer
// fails here rather than passing by accident — and cannot reach a hub that
// happens to be listening on the developer's own 8080 while doing so.
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

// TestTargetHostHeaderIsThePreservedHubAddress: dialing through the jail must
// not change what the hub is told it is. Target stays 127.0.0.1:8080 — the
// hub's address INSIDE the guest — so the Host header stays correct even
// though the connection is a pipe to a process.
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

// TestDialFailureIsAudited: a transport that cannot reach the hub must say
// why. A 502 with no recorded cause is indistinguishable from an auth
// failure or a wedged hub, which is precisely the diagnosis the jail
// transport's error messages exist to provide.
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

// TestSlowHubHeadersAreBounded: nothing else in the stack bounds getting a
// response out of the hub — the jail dial returns as soon as the child
// starts, and Serve builds an http.Server with no timeouts. Without the
// transport's header timeout a wedged machine holds the request open forever,
// producing no answer and no audit line. The failure must be bounded, a 502,
// and recorded with its cause.
func TestSlowHubHeadersAreBounded(t *testing.T) {
	// A listener that accepts and then says nothing: the shape of a jail that
	// connects but never answers.
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

// TestStreamedBodyOutlivesTheHeaderTimeout pins that the bound is on HEADERS
// only. The hub's transcript and attach streams send headers immediately and
// then dribble for as long as the operator watches; a timeout that covered
// the body would cut them off mid-stream.
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

	// Through a REAL listener, so Host is the loopback address httptest chose.
	// Set it to the tailnet name the Config serves, which is what `tailscale
	// serve` forwards — otherwise the Host gate refuses it (see hostAllowed).
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

// testServeHost is the tailnet name every test's Config uses, and therefore the
// Host that real traffic arrives with: `tailscale serve` forwards the client's
// Host unchanged for a TCP backend.
const testServeHost = "mac.ts.net"

// proxyRequest builds a request as it ARRIVES at the proxy. It exists because
// httptest.NewRequest defaults Host to "example.com", which is precisely the
// shape hostAllowed now refuses — a request asking for a name this proxy does
// not answer to. Tests that want to exercise a WRONG Host set req.Host
// themselves, explicitly.
func proxyRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Host = testServeHost
	return r
}

// TestHostGateRefusesDNSRebinding is the regression test for the hole an
// independent review found and I reproduced against the live proxy: with no
// Origin and no Sec-Fetch-Site, every other gate passes by default, so a
// rebound page on the attacker's own name reached the hub with the injected
// credential's authority. The browser cannot lie here — it sends the name the victim
// navigated to.
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
			// The rebind's full shape: no Origin, same-origin fetch metadata,
			// and a forged identity header.
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

// TestAuditFieldsAreBoundedOnEveryDecisionPath is the regression test for an
// unbounded write to the operator's disk. Path, method and Tailscale-User-Login
// are all chosen by the caller, and the gate audits DENIALS as well as allowed
// requests — lines written before any identity check has passed. Without a cap,
// anything that reaches the listener appends close to a megabyte per request
// (http.Server's default MaxHeaderBytes) to remote-audit.jsonl, which sits in
// the same state directory as the broker's own data. The provider's sink has
// always bounded its path; this is the same file.
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

// TestTheGateDecidesOnTheWholeLoginNotTheAuditCopy pins the split the bound
// depends on: the audit line carries a truncated COPY of the tailnet login,
// while every decision keeps using the value the front end actually sent.
// Deciding on the truncated one would make any two logins sharing their first
// maxAuditFieldLen bytes the same operator — one allowlist verdict and one hub
// session between them.
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

	// The hub session is keyed on the whole login too, so two operators can
	// never end up sharing one.
	if _, _, logins := sess.state(); len(logins) != 1 || logins[0] != allowed {
		t.Fatalf("the hub login was performed for %q, want the untruncated %q", logins, allowed)
	}
	// What lands in the log is still the bounded copy.
	for _, l := range sink.all() {
		if len(l.TSLogin) > maxAuditFieldLen+len("…") {
			t.Fatalf("audited ts_login is %d bytes, want at most %d", len(l.TSLogin), maxAuditFieldLen+len("…"))
		}
	}
}

// setResponseHeaderTimeout lowers the header wait on the Transport NewHandler
// built for a DialContext config, so a test can exercise the 502 path without
// waiting out the production bound. The seam is the built instance, not a
// Config field: production has no reason to set it.
func setResponseHeaderTimeout(t *testing.T, h http.Handler, d time.Duration) {
	t.Helper()
	tr, ok := h.(*gate).rp.Transport.(*http.Transport)
	if !ok {
		t.Fatal("handler has no jail transport — the config needs DialContext")
	}
	tr.ResponseHeaderTimeout = d
}

// TestHubRoutesTheProxyNeverForwards pins refusedRoute against scion's route
// table. /api/v1/system/* runs no RBAC (RouteWorkstation, loopback-only, and
// the proxy dials from loopback), so every route there is refused except the
// status read the SPA makes on load. Project create, register and clone make
// the caller an owner, and are refused whatever the hub would decide. The
// rows cover every /api/v1/system route in pkg/hub/route_metadata.go.
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

// TestClientIdentityHeadersStrippedFromForward: every header scion's auth
// middleware reads as an identity or credential is the proxy's to set or no
// one's. A client-supplied agent token, broker signature, trusted-proxy user
// set or federation token must never reach the hub beside the session.
func TestClientIdentityHeadersStrippedFromForward(t *testing.T) {
	hub := newRecordingHub(t)
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(), ServeHost: "mac.ts.net"})
	stripped := []string{
		"X-Scion-Agent-Token", "X-Scion-Broker-ID", "X-Scion-Broker-Token", "X-Scion-Signature",
		"X-Scion-Timestamp", "X-Scion-Nonce", "X-Scion-Signed-Headers", "X-Scion-On-Behalf-Of",
		"X-Scion-Federation-Token", "X-Scion-Plugin-Name",
		"X-Forwarded-User-Id", "X-Forwarded-User-Email", "X-Forwarded-User-Name", "X-Forwarded-User-Role",
		"X-Goog-IAP-JWT-Assertion", "X-API-Key",
	}
	req := proxyRequest("GET", "/api/v1/agents", nil)
	for _, k := range stripped {
		req.Header.Set(k, "forged")
	}
	req.Header.Set("x-scion-agent-token", "forged-lowercase")
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
		"X-Forwarded-For": false, "X-Forwarded-Proto": false, "Accept": false, "X-Preview-Token": false,
	} {
		if got := clientIdentityHeader(k); got != want {
			t.Errorf("clientIdentityHeader(%q) = %v, want %v", k, got, want)
		}
	}
}
