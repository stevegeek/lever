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

	"github.com/stevegeek/lever/internal/hubauth"
)

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

// scion turns the cookie into a bearer only when the request has no
// Authorization header, so any Authorization would decide the identity and
// the DM participant check would see a second user.
func TestAPIRequestRidesTheSession(t *testing.T) {
	hub := newRecordingHub(t)
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess, AllowedUsers: []string{"op@example.test"}})

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1"}, {http.MethodGet, "/api/v1/agents"},
		{http.MethodPost, "/api/v1/agents/x/messages"}, {http.MethodGet, "/api/v1/agents/x/pty"},
	} {
		req := proxyRequest(tc.method, tc.path, strings.NewReader(""))
		req.Header.Set("Tailscale-User-Login", "op@example.test")
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("%s %s = %d", tc.method, tc.path, rw.Code)
		}
	}
	for _, r := range hub.requests() {
		if c := r.Header.Get("Cookie"); c != sessionCookieName+"=sess-value" {
			t.Fatalf("%s carried Cookie %q, want the operator's session", r.URL.Path, c)
		}
		if a := r.Header.Values("Authorization"); len(a) != 0 {
			t.Fatalf("%s carried Authorization %q — it would override the session", r.URL.Path, a)
		}
	}
	if _, _, logins := sess.state(); len(logins) != 4 || logins[0] != "op@example.test" {
		t.Fatalf("session obtained for %v, want the verified tailnet login on every request", logins)
	}
}

func TestShellRequestCarriesOnlyTheSession(t *testing.T) {
	hub := newRecordingHub(t)
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess, AllowedUsers: []string{"op@example.test"}})

	for _, path := range []string{"/", "/auth/me", "/assets/main.js", "/events"} {
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
		if a := r.Header.Get("Authorization"); a != "" {
			t.Fatalf("%s carried Authorization %q", r.URL.Path, a)
		}
	}
	if _, _, logins := sess.state(); len(logins) == 0 || logins[0] != "op@example.test" {
		t.Fatalf("session obtained for %v, want the verified tailnet login", logins)
	}
}

// A browser holding its own scion_sess must not present it to the hub.
func TestClientSuppliedCookieIsReplacedNotMerged(t *testing.T) {
	hub := newRecordingHub(t)
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess})
	req := proxyRequest(http.MethodGet, "/", nil)
	req.Header.Set("Cookie", "scion_sess=attacker; other=1")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	got := hub.requests()[0].Header.Values("Cookie")
	if len(got) != 1 || got[0] != sessionCookieName+"=sess-value" {
		t.Fatalf("Cookie header = %v, want only the proxy's own session", got)
	}
}

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

// When the hub no longer knows the session, the operator must get the page,
// not scion's login screen.
func TestStaleSessionIsRenewedAndTheRequestRetried(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		location string
	}{
		{"401 for a programmatic request", http.StatusUnauthorized, ""},
		{"302 to the login page for a navigation", http.StatusFound, "/auth/login"},
		{"302 to the SPA login route", http.StatusFound, "/login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newRecordingHub(t)
			answer, calls := answerUnauthenticatedOnce(tc.status, tc.location)
			hub.answer = answer
			sess := &stubSession{cookie: "stale-session"}
			var audited []AuditLine
			h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess,
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

// A hub that rejects every session must not turn one request into a loop.
func TestRetryHappensOnlyOnce(t *testing.T) {
	hub := newRecordingHub(t)
	var calls atomic.Int32
	hub.answer = func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/", nil))
	if rw.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the hub's 401 to stand after one retry", rw.Code)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("hub saw %d requests, want 2", n)
	}
}

// A POST body is spent, so the session is dropped for next time and the hub's
// answer stands.
func TestOnlyBodilessMethodsAreRetried(t *testing.T) {
	hub := newRecordingHub(t)
	var calls atomic.Int32
	hub.answer = func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess})
	for _, path := range []string{"/upload", "/api/v1/agents/x/messages"} {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, proxyRequest(http.MethodPost, path, strings.NewReader("body")))
		if rw.Code != http.StatusUnauthorized {
			t.Fatalf("POST %s: status = %d, want the hub's answer", path, rw.Code)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("hub saw %d requests, want 2 — a POST must not be replayed", n)
	}
	// ...but the rejected session is dropped for the next request.
	if _, invalidated, _ := sess.state(); len(invalidated) != 2 || invalidated[0] != "sess-value" {
		t.Fatalf("invalidated = %v, want the rejected session dropped each time", invalidated)
	}
}

func TestAPIGetIsRetriedLikeTheShell(t *testing.T) {
	hub := newRecordingHub(t)
	answer, calls := answerUnauthenticatedOnce(http.StatusUnauthorized, "")
	hub.answer = answer
	sess := &stubSession{cookie: "stale-session"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/api/v1/agents", nil))
	if rw.Code != http.StatusOK || rw.Body.String() != "the real page" {
		t.Fatalf("status = %d body %q, want the retry's answer", rw.Code, rw.Body.String())
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("hub saw %d requests, want 2", n)
	}
	if _, invalidated, _ := sess.state(); len(invalidated) != 1 || invalidated[0] != "stale-session" {
		t.Fatalf("invalidated = %v", invalidated)
	}
}

// A 403 refuses an action; a new session would not change it.
func TestNonSessionFailuresArePassedThrough(t *testing.T) {
	hub := newRecordingHub(t)
	var calls atomic.Int32
	hub.answer = func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
	}
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess})
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

func TestRedirectsThatAreNotLoginPagesPassThrough(t *testing.T) {
	hub := newRecordingHub(t)
	hub.answer = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/agents/manager", http.StatusFound)
	}
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/", nil))
	if rw.Code != http.StatusFound || rw.Header().Get("Location") != "/agents/manager" {
		t.Fatalf("status = %d Location = %q, want the hub's own redirect", rw.Code, rw.Header().Get("Location"))
	}
}

// The login failure reason goes to the audit log, not the response.
func TestLoginFailureIs502NotAPage(t *testing.T) {
	hub := newRecordingHub(t)
	sess := &stubSession{err: errors.New("hub refused the callback (state_mismatch)")}
	var audited []AuditLine
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess, Audit: func(l AuditLine) { audited = append(audited, l) }})
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

// An API request rides the SAME session as the shell, so the hub sees one
// user for both.
func TestSessionAndProxyEndToEnd(t *testing.T) {
	hub := newFakeScionHub(t)
	d := newLoginDriverFor(t, hub, &testSigner{})
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", AllowedUsers: []string{"op@example.test"}, Session: d})

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
	if rw2 := get("/api/v1/agents"); rw2.Code != http.StatusOK || !strings.Contains(rw2.Body.String(), `"user":"op@example.test"`) {
		t.Fatalf("/api/v1/agents = %d %q, want it answered as the signed-in operator", rw2.Code, rw2.Body.String())
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.apiCalls) != 1 {
		t.Fatalf("%d API calls recorded, want 1", len(hub.apiCalls))
	}
	if a := hub.apiCalls[0].Get("Authorization"); a != "" {
		t.Fatalf("the API call carried Authorization %q", a)
	}
	if hub.logins != 1 {
		t.Fatalf("%d logins, want 1", hub.logins)
	}
}

func newLoginDriverFor(t *testing.T, hub *fakeScionHub, s *testSigner) *LoginDriver {
	t.Helper()
	return NewLoginDriver(LoginConfig{Hub: mustURL(t, hub.URL), Sign: s.sign, HubUserID: (&recordedUsers{}).lookup})
}

// A phone that sends its own assertion must not reach the hub with it on any
// path, the session retry included: the hub would take it as the caller's
// identity. Only the driver's own assertion, on its login request, arrives.
func TestAClientAssertionNeverReachesTheHub(t *testing.T) {
	hub := newFakeScionHub(t)
	signer := &testSigner{}
	d := newLoginDriverFor(t, hub, signer)
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", AllowedUsers: []string{"op@example.test"}, Session: d})

	forged := assertionPrefix + "admin@example.test.forged"
	send := func(method, path string) *httptest.ResponseRecorder {
		req := proxyRequest(method, path, nil)
		req.Header.Set("Tailscale-User-Login", "op@example.test")
		req.Header.Set(hubauth.Header, forged)
		req.Header.Add("x-lever-hub-assertion", forged+"2")
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		return rw
	}
	for _, path := range []string{"/", "/api/v1/agents", "/auth/me", "/auth/login"} {
		send(http.MethodGet, path)
	}
	// A stale session sends the GET through the retry path.
	hub.mu.Lock()
	hub.rejectSessions = true
	hub.mu.Unlock()
	send(http.MethodGet, "/api/v1/agents")
	send(http.MethodPost, "/api/v1/agents")

	_, issued := signer.all()
	ours := map[string]bool{}
	for _, a := range issued {
		ours[a] = true
	}
	seen := hub.seen()
	if len(seen) == 0 {
		t.Fatal("test bug: the driver never logged in")
	}
	for _, s := range seen {
		if !ours[s.value] || s.path != hubLoginPath || s.method != http.MethodGet {
			t.Fatalf("the hub saw an assertion that is not the driver's own login: %+v", s)
		}
	}
}

func TestEndToEndSessionSurvivesAHubRestart(t *testing.T) {
	hub := newFakeScionHub(t)
	d := newLoginDriverFor(t, hub, &testSigner{})
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: d})

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("first request = %d", rw.Code)
	}

	// As after scion server restarts for a config change.
	hub.mu.Lock()
	hub.sessions = map[string]string{}
	hub.mu.Unlock()

	rw2 := httptest.NewRecorder()
	h.ServeHTTP(rw2, proxyRequest(http.MethodGet, "/", nil))
	if rw2.Code != http.StatusOK {
		t.Fatalf("after the hub forgot the session = %d, want 200 via a fresh login", rw2.Code)
	}
	if logins := hub.loginCount(); logins != 2 {
		t.Fatalf("%d logins, want 2 (one per session)", logins)
	}
}

// The proxy answers a sign-in navigation itself: one driver run, a 302 back
// into the app, and nothing forwarded.
func TestSignInNavigationIsDrivenNotForwarded(t *testing.T) {
	hub := newRecordingHub(t)
	sess := &stubSession{cookie: "sess-value"}
	var audited []AuditLine
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess, Audit: func(l AuditLine) { audited = append(audited, l) }})

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

// An in-app returnTo is honoured; anything that could leave the origin falls
// back to "/".
func TestSignInNavigationPreservesReturnTarget(t *testing.T) {
	hub := newRecordingHub(t)
	sess := &stubSession{cookie: "sess-value"}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess})

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

// The callback is the driver's to call directly; through the proxy it could
// only force login churn, so it is refused for every method.
func TestCallbackIsRefused(t *testing.T) {
	for _, target := range []string{"/auth/callback/oidc?code=x&state=y", "/auth/callback/x", "/auth/callback", "/auth/%63allback/oidc"} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
			hub := newRecordingHub(t)
			sess := &stubSession{cookie: "sess-value"}
			var audited []AuditLine
			h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess,
				Audit: func(l AuditLine) { audited = append(audited, l) }})
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, proxyRequest(method, target, nil))
			if rw.Code != http.StatusForbidden || len(hub.requests()) != 0 {
				t.Fatalf("%s %s: status %d, hub saw %d; want 403 and nothing forwarded", method, target, rw.Code, len(hub.requests()))
			}
			if len(audited) != 1 || audited[0].Decision != DecisionDenyRoute {
				t.Fatalf("%s %s: audit = %+v, want one deny-route line", method, target, audited)
			}
			if handed, invalidated, _ := sess.state(); handed != 0 || len(invalidated) != 0 {
				t.Fatalf("%s %s: session touched (handed %d, invalidated %v)", method, target, handed, invalidated)
			}
		}
	}
}

// scion sends /login?error= only from its OAuth callback. Treating it as an
// unknown session would let a client force a login per request.
func TestLoginRedirectWithErrorIsNotASessionRejection(t *testing.T) {
	for _, loc := range []string{"/login?error=state_mismatch", "/login?error=session_error", "/auth/login?error=x", "https://evil.example/login", "/loginx"} {
		hub := newRecordingHub(t)
		var calls atomic.Int32
		hub.answer = func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Location", loc)
			w.WriteHeader(http.StatusFound)
		}
		sess := &stubSession{cookie: "sess-value"}
		h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess})
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, proxyRequest(method, "/some/page", strings.NewReader("")))
			if rw.Code != http.StatusFound || rw.Header().Get("Location") != loc {
				t.Fatalf("%s Location %q: status %d Location %q, want the hub's redirect passed through", method, loc, rw.Code, rw.Header().Get("Location"))
			}
		}
		if n := calls.Load(); n != 2 {
			t.Fatalf("Location %q: hub saw %d requests, want 2 (no retry)", loc, n)
		}
		if _, invalidated, _ := sess.state(); len(invalidated) != 0 {
			t.Fatalf("Location %q: session invalidated %v", loc, invalidated)
		}
	}
}

// The browser gets a clear failure, never the hub's outward redirect.
func TestSignInNavigationLoginFailureIs502(t *testing.T) {
	hub := newRecordingHub(t)
	sess := &stubSession{err: errors.New("hub refused the callback (state_mismatch)")}
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess})

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

// The driver's login reaches the hub directly, so the navigation makes
// exactly one login, and the browser lands back in the app.
func TestSignInEndToEndLogsInOnce(t *testing.T) {
	hub := newFakeScionHub(t)
	d := newLoginDriverFor(t, hub, &testSigner{})
	h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: d})

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/auth/login/oidc", nil))
	if rw.Code != http.StatusFound || rw.Header().Get("Location") != "/" {
		t.Fatalf("status = %d Location = %q, want 302 to /", rw.Code, rw.Header().Get("Location"))
	}
	if logins := hub.loginCount(); logins != 1 {
		t.Fatalf("%d login(s), want exactly 1", logins)
	}

	rw2 := httptest.NewRecorder()
	h.ServeHTTP(rw2, proxyRequest(http.MethodGet, "/", nil))
	if rw2.Code != http.StatusOK {
		t.Fatalf("shell after sign-in = %d, want 200", rw2.Code)
	}
	if logins := hub.loginCount(); logins != 1 {
		t.Fatalf("shell request logged in again (%d logins) — the session was not reused", logins)
	}
}

// The denial text points at Config.LogPath, or DefaultLogPath when unset.
func TestLoginFailureNamesTheConfiguredLog(t *testing.T) {
	hub := newRecordingHub(t)
	for _, tc := range []struct{ logPath, want string }{
		{"", "hub login failed — see .lever-state/remote.log"},
		{"/srv/x/.lever-state/remote.log", "hub login failed — see /srv/x/.lever-state/remote.log"},
	} {
		sess := &stubSession{err: errors.New("hub refused")}
		h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess, LogPath: tc.logPath})
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, proxyRequest("GET", "/", nil))
		if rw.Code != http.StatusBadGateway {
			t.Fatalf("LogPath %q: status = %d, want 502", tc.logPath, rw.Code)
		}
		if got := strings.TrimSpace(rw.Body.String()); got != tc.want {
			t.Fatalf("LogPath %q: body = %q, want %q", tc.logPath, got, tc.want)
		}
	}
}

// With allowed_users empty the gate verifies nothing about
// Tailscale-User-Login, so the hub login is for the unnamed operator, never
// the header's claim. With it set, the gated login is what the hub records.
func TestUnpinnedIdentityIsNeverAssertedToTheHub(t *testing.T) {
	cases := []struct {
		name      string
		allowed   []string
		header    string
		wantLogin string
	}{
		{"empty list, header ignored", nil, "someone@example.test", ""},
		{"empty list, no header", nil, "", ""},
		{"pinned, the verified login is asserted", []string{"op@example.test"}, "op@example.test", "op@example.test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newRecordingHub(t)
			sess := &stubSession{cookie: "sess-value"}
			h := NewHandler(Config{Target: mustURL(t, hub.URL), ServeHost: "mac.ts.net", Session: sess, AllowedUsers: tc.allowed})
			for _, path := range []string{"/", loginPathPrefix} {
				req := proxyRequest(http.MethodGet, path, nil)
				if tc.header != "" {
					req.Header.Set("Tailscale-User-Login", tc.header)
				}
				rw := httptest.NewRecorder()
				h.ServeHTTP(rw, req)
				if rw.Code != http.StatusOK && rw.Code != http.StatusFound {
					t.Fatalf("GET %s = %d", path, rw.Code)
				}
			}
			_, _, logins := sess.state()
			if len(logins) == 0 {
				t.Fatal("no login was driven")
			}
			for _, l := range logins {
				if l != tc.wantLogin {
					t.Fatalf("the session was requested for %q, want %q", l, tc.wantLogin)
				}
			}
		})
	}
}
