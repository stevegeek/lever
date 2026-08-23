package remoteproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// stubSession is a SessionSource with no hub behind it, so the proxy's own
// rules can be exercised on their own.
type stubSession struct {
	mu          sync.Mutex
	cookie      string
	err         error
	handedOut   int
	invalidated []string
	logins      []string // the operator login each Cookie call was made for
}

func (s *stubSession) Cookie(_ context.Context, login string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logins = append(s.logins, login)
	if s.err != nil {
		return "", s.err
	}
	s.handedOut++
	return s.cookie, nil
}

func (s *stubSession) Invalidate(_, cookie string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated = append(s.invalidated, cookie)
}

func (s *stubSession) state() (handedOut int, invalidated []string, logins []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handedOut, append([]string(nil), s.invalidated...), append([]string(nil), s.logins...)
}

// TestSessionNeverRidesAnAPIRequest is the least-privilege property: /api/v1
// keeps riding the narrow remote PAT, and only the UI shell gets the hub
// session. scion's sessionToBearerMiddleware passes a request through when it
// carries an Authorization header, so a session sent alongside the PAT would
// be inert TODAY — but it would be a second, WIDER credential travelling to
// the API on every request, one middleware change away from being honored.
func TestSessionNeverRidesAnAPIRequest(t *testing.T) {
	hub := newRecordingHub(t)
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
		ServeHost: "mac.ts.net", Session: sess})

	for _, path := range []string{"/api/v1", "/api/v1/agents", "/api/v1/agents/x/messages"} {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, proxyRequest(http.MethodGet, path, nil))
		if rw.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rw.Code)
		}
	}
	for _, r := range hub.requests() {
		if c := r.Header.Get("Cookie"); c != "" {
			t.Fatalf("%s carried a session cookie (%q) — the API must ride the narrow PAT alone", r.URL.Path, c)
		}
		if a := r.Header.Get("Authorization"); a != "Bearer scion_pat_x" {
			t.Fatalf("%s carried Authorization %q", r.URL.Path, a)
		}
	}
	if handed, _, _ := sess.state(); handed != 0 {
		t.Fatalf("the proxy obtained a session for API-only traffic (%d times) — that logs in for nothing", handed)
	}
}

// TestShellRequestCarriesTheSessionAndThePAT: the shell needs the cookie
// (scion's web layer reads nothing else); the PAT stays on the request because
// it is what every /api/v1 call the SPA then makes will ride.
func TestShellRequestCarriesTheSessionAndThePAT(t *testing.T) {
	hub := newRecordingHub(t)
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
		ServeHost: "mac.ts.net", Session: sess, AllowedUsers: []string{"op@example.test"}})

	for _, path := range []string{"/", "/auth/me", "/assets/main.js"} {
		req := proxyRequest(http.MethodGet, path, nil)
		req.Header.Set("Tailscale-User-Login", "op@example.test")
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rw.Code)
		}
	}
	for _, r := range hub.requests() {
		if c := r.Header.Get("Cookie"); c != sessionCookieName+"=sess-value" {
			t.Fatalf("%s carried Cookie %q", r.URL.Path, c)
		}
		if a := r.Header.Get("Authorization"); a != "Bearer scion_pat_x" {
			t.Fatalf("%s carried Authorization %q", r.URL.Path, a)
		}
	}
	// The session is obtained for the identity the proxy verified, not for
	// whoever the hub might think is connected.
	if _, _, logins := sess.state(); len(logins) == 0 || logins[0] != "op@example.test" {
		t.Fatalf("session obtained for %v, want the verified tailnet login", logins)
	}
}

// TestClientSuppliedCookieIsReplacedNotMerged: the client's own Cookie header
// is stripped before ours is attached, so a browser holding a scion_sess of
// its own cannot present it to the hub.
func TestClientSuppliedCookieIsReplacedNotMerged(t *testing.T) {
	hub := newRecordingHub(t)
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
		ServeHost: "mac.ts.net", Session: sess})
	req := proxyRequest(http.MethodGet, "/", nil)
	req.Header.Set("Cookie", "scion_sess=attacker; other=1")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	got := hub.requests()[0].Header.Values("Cookie")
	if len(got) != 1 || got[0] != sessionCookieName+"=sess-value" {
		t.Fatalf("Cookie header = %v, want only the proxy's own session", got)
	}
}

// answerUnauthenticatedOnce answers the first request the way scion answers an
// unknown session, and normally thereafter.
func answerUnauthenticatedOnce(status int, location string) (func(http.ResponseWriter, *http.Request), *atomic.Int32) {
	var n atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			if location != "" {
				w.Header().Set("Location", location)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"authentication required"}`))
			return
		}
		_, _ = w.Write([]byte("the real page"))
	}, &n
}

// TestStaleSessionIsRenewedAndTheRequestRetried: when the hub no longer knows
// the session (it restarted, or the session lapsed), the operator must get
// their page — not scion's login screen, whose "sign in" button leads to an
// authorization endpoint that does not resolve, by design.
func TestStaleSessionIsRenewedAndTheRequestRetried(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		location string
	}{
		{"401 for a programmatic request", http.StatusUnauthorized, ""},
		{"302 to the login page for a navigation", http.StatusFound, "/auth/login"},
		{"302 to the SPA login route", http.StatusFound, "/login?error=session_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newRecordingHub(t)
			answer, calls := answerUnauthenticatedOnce(tc.status, tc.location)
			hub.answer = answer
			sess := &stubSession{cookie: "stale-session"}
			var audited []AuditLine
			h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
				ServeHost: "mac.ts.net", Session: sess,
				Audit: func(l AuditLine) { audited = append(audited, l) }})

			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/", nil))

			if rw.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — the retry's answer, not the hub's rejection", rw.Code)
			}
			if body := rw.Body.String(); body != "the real page" {
				t.Fatalf("body = %q, want only the retry's response", body)
			}
			if loc := rw.Header().Get("Location"); loc != "" {
				t.Fatalf("Location %q leaked from the swallowed rejection", loc)
			}
			if n := calls.Load(); n != 2 {
				t.Fatalf("hub saw %d requests, want 2 (the original and the retry)", n)
			}
			_, invalidated, _ := sess.state()
			if len(invalidated) != 1 || invalidated[0] != "stale-session" {
				t.Fatalf("invalidated = %v, want exactly the session the hub rejected", invalidated)
			}
			if len(audited) != 1 || audited[0].Status != http.StatusOK {
				t.Fatalf("audit = %+v, want exactly one line carrying the answer the client got", audited)
			}
		})
	}
}

// TestRetryHappensOnlyOnce stops a hub that rejects every session from turning
// one request into a login loop.
func TestRetryHappensOnlyOnce(t *testing.T) {
	hub := newRecordingHub(t)
	var calls atomic.Int32
	hub.answer = func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
		ServeHost: "mac.ts.net", Session: sess})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/", nil))
	if rw.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the hub's 401 to stand after one retry", rw.Code)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("hub saw %d requests, want 2", n)
	}
}

// TestOnlyBodilessMethodsAreRetried: a POST cannot be replayed (its body is
// spent), so the session is dropped for next time and the hub's answer stands.
func TestOnlyBodilessMethodsAreRetried(t *testing.T) {
	hub := newRecordingHub(t)
	var calls atomic.Int32
	hub.answer = func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
		ServeHost: "mac.ts.net", Session: sess})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodPost, "/upload", strings.NewReader("body")))
	if rw.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the hub's answer", rw.Code)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("hub saw %d requests, want 1 — a POST must not be replayed", n)
	}
}

// TestNonSessionFailuresArePassedThrough: a 403 is the hub refusing an action.
// A new session would not change it, and retrying would hide it.
func TestNonSessionFailuresArePassedThrough(t *testing.T) {
	hub := newRecordingHub(t)
	var calls atomic.Int32
	hub.answer = func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
	}
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
		ServeHost: "mac.ts.net", Session: sess})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/", nil))
	if rw.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 passed through", rw.Code)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("hub saw %d requests, want 1", n)
	}
	if _, invalidated, _ := sess.state(); len(invalidated) != 0 {
		t.Fatalf("a 403 discarded the session: %v", invalidated)
	}
}

// TestRedirectsThatAreNotLoginPagesPassThrough: the SPA's own redirects must
// reach the browser untouched.
func TestRedirectsThatAreNotLoginPagesPassThrough(t *testing.T) {
	hub := newRecordingHub(t)
	hub.answer = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/agents/manager", http.StatusFound)
	}
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
		ServeHost: "mac.ts.net", Session: sess})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/", nil))
	if rw.Code != http.StatusFound || rw.Header().Get("Location") != "/agents/manager" {
		t.Fatalf("status = %d Location = %q, want the hub's own redirect", rw.Code, rw.Header().Get("Location"))
	}
}

// TestLoginFailureIs502NotAPage: an operator who cannot be logged in must get
// a clear failure, and the reason must land in the audit log rather than in
// the response.
func TestLoginFailureIs502NotAPage(t *testing.T) {
	hub := newRecordingHub(t)
	sess := &stubSession{err: errors.New("hub refused the callback (state_mismatch)")}
	var audited []AuditLine
	h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
		ServeHost: "mac.ts.net", Session: sess, Audit: func(l AuditLine) { audited = append(audited, l) }})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/", nil))
	if rw.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rw.Code)
	}
	if len(hub.requests()) != 0 {
		t.Fatal("a request reached the hub with no session")
	}
	if len(audited) != 1 || audited[0].Decision != "deny-no-session" {
		t.Fatalf("audit = %+v, want one deny-no-session line", audited)
	}
}

// TestNoSessionSourceMeansNoCookie keeps the API-only posture (and every
// pre-existing caller) working unchanged when no provider is configured.
func TestNoSessionSourceMeansNoCookie(t *testing.T) {
	hub := newRecordingHub(t)
	h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
		ServeHost: "mac.ts.net"})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d", rw.Code)
	}
	if c := hub.requests()[0].Header.Get("Cookie"); c != "" {
		t.Fatalf("Cookie = %q, want none", c)
	}
}

func TestIsAPIPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/v1":              true,
		"/api/v1/":             true,
		"/api/v1/agents":       true,
		"/api/v1x":             false,
		"/api/v1x/agents":      false,
		"/":                    false,
		"/auth/me":             false,
		"/assets/api/v1":       false,
		"/api":                 false,
		"/api/v2/agents":       false,
		"/prefix/api/v1/agent": false,
	} {
		if got := isAPIPath(path); got != want {
			t.Fatalf("isAPIPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestSessionAndProxyEndToEnd runs the real driver and provider behind the
// real handler against the fake hub: a browser request opens the shell, an API
// request rides the PAT, and the hub sees a session only on the first.
func TestSessionAndProxyEndToEnd(t *testing.T) {
	p, _, _ := startProvider(t)
	hub := newFakeScionHub(t, p.IssuerURL(), "scion_pat_x")
	d := NewLoginDriver(LoginConfig{Hub: mustURL(t, hub.URL), Provider: p})
	h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
		ServeHost: "mac.ts.net", AllowedUsers: []string{"op@example.test"}, Session: d})

	get := func(path string) *httptest.ResponseRecorder {
		req := proxyRequest(http.MethodGet, path, nil)
		req.Header.Set("Tailscale-User-Login", "op@example.test")
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		return rw
	}

	rw := get("/auth/me")
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), "op@example.test") {
		t.Fatalf("/auth/me = %d %q, want the logged-in shell", rw.Code, rw.Body.String())
	}
	if rw.Header().Get("Set-Cookie") != "" {
		t.Fatal("the hub's session cookie reached the client")
	}
	if rw2 := get("/api/v1/agents"); rw2.Code != http.StatusOK || !strings.Contains(rw2.Body.String(), `"ok":true`) {
		t.Fatalf("/api/v1/agents = %d %q", rw2.Code, rw2.Body.String())
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.apiCalls) != 1 {
		t.Fatalf("%d API calls recorded, want 1", len(hub.apiCalls))
	}
	if c := hub.apiCalls[0].Get("Cookie"); c != "" {
		t.Fatalf("the API call carried a session cookie: %q", c)
	}
	if hub.logins != 1 {
		t.Fatalf("%d logins, want 1", hub.logins)
	}
}

// TestEndToEndSessionSurvivesAHubRestart drives the real driver through the
// renewal path: the hub forgets every session, then accepts logins again.
func TestEndToEndSessionSurvivesAHubRestart(t *testing.T) {
	p, _, _ := startProvider(t)
	hub := newFakeScionHub(t, p.IssuerURL(), "scion_pat_x")
	d := NewLoginDriver(LoginConfig{Hub: mustURL(t, hub.URL), Provider: p})
	h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
		ServeHost: "mac.ts.net", Session: d})

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("first request = %d", rw.Code)
	}

	// Everything the hub knew is gone, exactly as after `scion server` is
	// restarted for a config change.
	hub.mu.Lock()
	hub.sessions = map[string]string{}
	hub.mu.Unlock()

	rw2 := httptest.NewRecorder()
	h.ServeHTTP(rw2, proxyRequest(http.MethodGet, "/", nil))
	if rw2.Code != http.StatusOK {
		t.Fatalf("after the hub forgot the session = %d, want 200 via a fresh login", rw2.Code)
	}
	if logins, _ := hub.counts(); logins != 2 {
		t.Fatalf("%d logins, want 2 (one per session)", logins)
	}
}

// TestSignInNavigationIsDrivenNotForwarded: the SPA's Sign-in button
// navigates to /auth/login/<provider>, and the hub's answer to that is a 302
// to the OIDC authorization endpoint — which deliberately does not resolve.
// The proxy must answer the navigation itself: one driver run, then a 302
// back into the app, and the hub never sees the request.
func TestSignInNavigationIsDrivenNotForwarded(t *testing.T) {
	hub := newRecordingHub(t)
	sess := &stubSession{cookie: "sess-value"}
	var audited []AuditLine
	h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
		ServeHost: "mac.ts.net", Session: sess, Audit: func(l AuditLine) { audited = append(audited, l) }})

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/auth/login/oidc", nil))
	if rw.Code != http.StatusFound || rw.Header().Get("Location") != "/" {
		t.Fatalf("status = %d Location = %q, want 302 to /", rw.Code, rw.Header().Get("Location"))
	}
	if n := len(hub.requests()); n != 0 {
		t.Fatalf("%d request(s) reached the hub — a sign-in navigation must never be forwarded", n)
	}
	if handed, _, _ := sess.state(); handed != 1 {
		t.Fatalf("driver ran %d time(s), want exactly 1", handed)
	}
	if len(audited) != 1 || audited[0].Decision != "login-redirect" {
		t.Fatalf("audit = %+v, want one login-redirect line", audited)
	}

	// The bare path — the shell's own Sign-in link — is intercepted too.
	rw2 := httptest.NewRecorder()
	h.ServeHTTP(rw2, proxyRequest(http.MethodGet, "/auth/login", nil))
	if rw2.Code != http.StatusFound || rw2.Header().Get("Location") != "/" {
		t.Fatalf("bare path: status = %d Location = %q, want 302 to /", rw2.Code, rw2.Header().Get("Location"))
	}
	if n := len(hub.requests()); n != 0 {
		t.Fatalf("%d request(s) reached the hub via the bare login path", n)
	}
}

// TestSignInNavigationPreservesReturnTarget: the SPA's login page forwards
// its own target as ?returnTo=<path>; the proxy honours an in-app path and
// falls back to "/" for anything that could redirect off-origin.
func TestSignInNavigationPreservesReturnTarget(t *testing.T) {
	hub := newRecordingHub(t)
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
		ServeHost: "mac.ts.net", Session: sess})

	cases := []struct{ returnTo, want string }{
		{"/agents/manager", "/agents/manager"},
		{"/invite", "/invite"},
		{"", "/"},
		{"agents", "/"},              // not an absolute path
		{"//evil.test/x", "/"},       // protocol-relative
		{"/\\evil.test", "/"},        // backslash reads as a slash in browsers
		{"https://evil.test/x", "/"}, // absolute URL
		{"javascript:alert(1)", "/"}, // scheme smuggling
	}
	for _, c := range cases {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/auth/login/oidc?returnTo="+url.QueryEscape(c.returnTo), nil))
		if rw.Code != http.StatusFound || rw.Header().Get("Location") != c.want {
			t.Fatalf("returnTo=%q: status = %d Location = %q, want 302 to %q",
				c.returnTo, rw.Code, rw.Header().Get("Location"), c.want)
		}
	}
	if n := len(hub.requests()); n != 0 {
		t.Fatalf("%d request(s) reached the hub", n)
	}
}

// TestCallbackIsForwardedNotIntercepted: /auth/callback/ is the hub's own to
// receive — the login driver hands the hub its callback through the ordinary
// upstream route, and intercepting it would break that handshake.
func TestCallbackIsForwardedNotIntercepted(t *testing.T) {
	hub := newRecordingHub(t)
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
		ServeHost: "mac.ts.net", Session: sess})

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/auth/callback/oidc?code=x&state=y", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want the hub's own 200", rw.Code)
	}
	reqs := hub.requests()
	if len(reqs) != 1 || reqs[0].URL.Path != "/auth/callback/oidc" {
		t.Fatalf("hub saw %+v, want exactly the callback", reqs)
	}
}

// TestSignInNavigationLoginFailureIs502: when the driver cannot log in, the
// browser gets a clear failure — never the hub's outward redirect.
func TestSignInNavigationLoginFailureIs502(t *testing.T) {
	hub := newRecordingHub(t)
	sess := &stubSession{err: errors.New("hub refused the callback (state_mismatch)")}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
		ServeHost: "mac.ts.net", Session: sess})

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/auth/login/oidc", nil))
	if rw.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rw.Code)
	}
	if loc := rw.Header().Get("Location"); loc != "" {
		t.Fatalf("Location = %q, want none on a failed sign-in", loc)
	}
	if n := len(hub.requests()); n != 0 {
		t.Fatalf("%d request(s) reached the hub", n)
	}
}

// TestSignInEndToEndNeverLeaksTheDeadEndpoint drives the REAL login driver
// against the scripted hub: the intercepted navigation triggers exactly one
// handshake (the driver's own step-1 GET reaches the hub directly, not
// through the handler — no recursion), the browser is sent to "/", and no
// response ever carries the dead authorization endpoint.
func TestSignInEndToEndNeverLeaksTheDeadEndpoint(t *testing.T) {
	p, _, _ := startProvider(t)
	hub := newFakeScionHub(t, p.IssuerURL(), "scion_pat_x")
	d := NewLoginDriver(LoginConfig{Hub: mustURL(t, hub.URL), Provider: p})
	h := NewHandler(Config{Target: mustURL(t, hub.URL), PAT: testPAT,
		ServeHost: "mac.ts.net", Session: d})

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/auth/login/oidc", nil))
	if rw.Code != http.StatusFound || rw.Header().Get("Location") != "/" {
		t.Fatalf("status = %d Location = %q, want 302 to /", rw.Code, rw.Header().Get("Location"))
	}
	if strings.Contains(rw.Header().Get("Location"), "lever.invalid") {
		t.Fatal("the dead authorization endpoint reached the browser")
	}
	if logins, _ := hub.counts(); logins != 1 {
		t.Fatalf("%d login handshake(s), want exactly 1", logins)
	}

	// The session the sign-in obtained is the one the shell now rides.
	rw2 := httptest.NewRecorder()
	h.ServeHTTP(rw2, proxyRequest(http.MethodGet, "/", nil))
	if rw2.Code != http.StatusOK {
		t.Fatalf("shell after sign-in = %d, want 200", rw2.Code)
	}
	if logins, _ := hub.counts(); logins != 1 {
		t.Fatalf("shell request logged in again (%d handshakes) — the session was not reused", logins)
	}
}
