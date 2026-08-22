package remoteproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

// startProvider serves a Provider on a real loopback listener whose port the
// provider knows, so its discovery document advertises endpoints that actually
// resolve — the same arrangement as production, where the guest forwarder
// makes 127.0.0.1:<port> mean this listener from inside the jail too.
func startProvider(t *testing.T) (*Provider, *auditSink, *httptest.Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	sink := &auditSink{}
	p := NewProvider(ProviderConfig{Port: port, Audit: sink.add})
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: p.Handler()}}
	srv.Start()
	t.Cleanup(srv.Close)
	if srv.URL != p.IssuerURL() {
		t.Fatalf("provider serves %s but advertises issuer %s", srv.URL, p.IssuerURL())
	}
	return p, sink, srv
}

// fakeScionHub models the parts of scion's web layer this handshake touches,
// as read from pkg/hub/web.go and pkg/hub/oauth.go at pin e82a2a08:
//
//   - ONE cookie, scion_sess, carrying the login state first and the session
//     afterwards;
//   - a redirect_uri built from the hub's own base_url — which is the hub's
//     localhost default, NOT an address this proxy can reach, so the driver
//     has to rewrite the origin;
//   - a callback that constant-time compares the state, then calls the
//     provider's /token and /userinfo back-channel itself;
//   - a web shell that answers 401 (or a redirect to the login page) without a
//     session, and an API that passes a request through untouched when it
//     carries an Authorization header.
type fakeScionHub struct {
	*httptest.Server
	issuer   string // the configured oidc_login.issuer_url
	clientID string
	pat      string // the narrow remote PAT the proxy injects

	// baseURL is what the hub builds redirect_uri from. Deliberately not its
	// own test address: scion defaults to http://localhost:<web-port>.
	baseURL string

	mu        sync.Mutex
	states    map[string]string // state cookie value -> state
	sessions  map[string]string // session cookie value -> email
	logins    int
	callbacks int
	apiCalls  []http.Header // headers of every /api/v1 request

	// knobs
	dropState        bool   // "forget" the state, as a lost cookie jar would
	authzOverride    string // redirect somewhere other than our provider
	clientIDOverride string // ask for a client_id lever did not configure
	rejectSessions   bool   // treat every session as unknown (a restarted hub)
	beforeCallback   func() // runs at the top of the callback, to hold a handshake open
	callbackFails    bool   // answer the callback 500, leaving the state cookie in place
}

func newFakeScionHub(t *testing.T, issuer, pat string) *fakeScionHub {
	t.Helper()
	h := &fakeScionHub{
		issuer:   issuer,
		clientID: LoginClientID,
		pat:      pat,
		baseURL:  "http://localhost:8080",
		states:   map[string]string{},
		sessions: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login/oidc", h.handleLogin)
	mux.HandleFunc("/auth/callback/oidc", h.handleCallback)
	mux.HandleFunc("/api/v1/", h.handleAPI)
	mux.HandleFunc("/auth/me", h.handleShell)
	mux.HandleFunc("/", h.handleShell)
	h.Server = httptest.NewServer(mux)
	t.Cleanup(h.Server.Close)
	return h
}

func (h *fakeScionHub) discover() (map[string]any, error) {
	resp, err := http.Get(h.issuer + "/.well-known/openid-configuration")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var doc map[string]any
	return doc, json.NewDecoder(resp.Body).Decode(&doc)
}

func randHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (h *fakeScionHub) handleLogin(w http.ResponseWriter, r *http.Request) {
	doc, err := h.discover()
	if err != nil {
		http.Error(w, "failed to generate auth URL", http.StatusInternalServerError)
		return
	}
	authz, _ := doc["authorization_endpoint"].(string)
	if h.authzOverride != "" {
		authz = h.authzOverride
	}
	clientID := h.clientID
	if h.clientIDOverride != "" {
		clientID = h.clientIDOverride
	}
	state, cookie := randHex(), randHex()
	h.mu.Lock()
	h.logins++
	if !h.dropState {
		h.states[cookie] = state
	}
	h.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: cookie, Path: "/"})
	q := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {h.baseURL + "/auth/callback/oidc"},
		"response_type": {"code"},
		"scope":         {"openid email profile"},
		"state":         {state},
	}
	http.Redirect(w, r, authz+"?"+q.Encode(), http.StatusFound)
}

func (h *fakeScionHub) handleCallback(w http.ResponseWriter, r *http.Request) {
	if h.beforeCallback != nil {
		h.beforeCallback()
	}
	h.mu.Lock()
	h.callbacks++
	h.mu.Unlock()
	if h.callbackFails {
		// A hub that neither redirects to its login page nor writes a session:
		// the jar keeps the cookie it already had, which is the login STATE.
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		http.Redirect(w, r, "/login?error=session_error", http.StatusFound)
		return
	}
	h.mu.Lock()
	want := h.states[c.Value]
	h.mu.Unlock()
	if want == "" || want != r.URL.Query().Get("state") {
		http.Redirect(w, r, "/login?error=state_mismatch", http.StatusFound)
		return
	}
	email, err := h.exchange(r.URL.Query().Get("code"))
	if err != nil {
		http.Redirect(w, r, "/login?error=exchange_failed", http.StatusFound)
		return
	}
	sess := randHex()
	h.mu.Lock()
	h.sessions[sess] = email
	h.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: sess, Path: "/"})
	http.Redirect(w, r, "/", http.StatusFound)
}

// exchange is the hub's back channel: POST /token, then GET /userinfo. It
// re-sends the SAME redirect_uri it issued, which is what binds the code.
func (h *fakeScionHub) exchange(code string) (string, error) {
	doc, err := h.discover()
	if err != nil {
		return "", err
	}
	tokenEP, _ := doc["token_endpoint"].(string)
	userinfoEP, _ := doc["userinfo_endpoint"].(string)
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {h.baseURL + "/auth/callback/oidc"},
		"client_id":    {h.clientID},
	}
	resp, err := http.PostForm(tokenEP, form)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("token exchange failed: %s %s", resp.Status, body)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodGet, userinfoEP, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	uresp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = uresp.Body.Close() }()
	if uresp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("userinfo returned %s", uresp.Status)
	}
	var info struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := json.NewDecoder(uresp.Body).Decode(&info); err != nil {
		return "", err
	}
	if info.Sub == "" || info.Email == "" || !info.EmailVerified {
		return "", fmt.Errorf("provider returned an unusable identity")
	}
	return info.Email, nil
}

// handleShell is the session-gated web surface.
func (h *fakeScionHub) handleShell(w http.ResponseWriter, r *http.Request) {
	if email, ok := h.sessionEmail(r); ok {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "shell for %s", email)
		return
	}
	// scion redirects a browser navigation to its login page and answers
	// anything else 401.
	if r.URL.Path == "/" {
		http.Redirect(w, r, "/auth/login", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"authentication required"}`))
}

func (h *fakeScionHub) sessionEmail(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "", false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rejectSessions {
		return "", false
	}
	email, ok := h.sessions[c.Value]
	return email, ok
}

// handleAPI models sessionToBearerMiddleware: a request carrying an
// Authorization header passes through to the hub's own auth, which is where
// the narrow PAT is checked.
func (h *fakeScionHub) handleAPI(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.apiCalls = append(h.apiCalls, r.Header.Clone())
	h.mu.Unlock()
	if r.Header.Get("Authorization") == "Bearer "+h.pat {
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	if _, ok := h.sessionEmail(r); ok {
		// A session would ALSO open the API — which is exactly why the proxy
		// must never send one here.
		_, _ = w.Write([]byte(`{"ok":true,"via":"session"}`))
		return
	}
	w.WriteHeader(http.StatusUnauthorized)
}

func (h *fakeScionHub) counts() (logins, callbacks int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logins, h.callbacks
}

// newTestDriver wires a driver at a hub, both fake, with the provider served
// on its own listener.
func newTestDriver(t *testing.T) (*LoginDriver, *fakeScionHub, *Provider, *auditSink) {
	t.Helper()
	p, psink, _ := startProvider(t)
	hub := newFakeScionHub(t, p.IssuerURL(), "scion_pat_x")
	sink := &auditSink{}
	d := NewLoginDriver(LoginConfig{Hub: mustURL(t, hub.URL), Provider: p, Audit: sink.add})
	return d, hub, p, psink
}

// TestHandshakeCompletesWithNoAuthorizeRouteRegistered is the decisive test
// for the whole design: a full hub login, driven server-side, while the
// provider has no authorization endpoint at all.
func TestHandshakeCompletesWithNoAuthorizeRouteRegistered(t *testing.T) {
	d, hub, p, psink := newTestDriver(t)

	cookie, err := d.Cookie(context.Background(), "op@example.test")
	if err != nil {
		t.Fatalf("Cookie: %v", err)
	}
	if cookie == "" {
		t.Fatal("handshake returned an empty session")
	}

	// The session actually opens the hub's shell.
	req, _ := http.NewRequest(http.MethodGet, hub.URL+"/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /auth/me: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/auth/me = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "op@example.test") {
		t.Fatalf("/auth/me = %q, want the operator the proxy verified", body)
	}

	// The provider answered ONLY the three endpoints the hub calls. If
	// /authorize appears here, the login has grown a code-minting endpoint
	// reachable from inside the jail — see TestAuthorizeIsPermanently404.
	var paths []string
	for _, l := range psink.all() {
		paths = append(paths, l.Path)
	}
	want := []string{"/.well-known/openid-configuration", "/.well-known/openid-configuration", "/token", "/userinfo"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("provider served %v, want %v", paths, want)
	}
	resp2, err := http.Get(p.IssuerURL() + "/authorize")
	if err != nil {
		t.Fatalf("GET /authorize: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /authorize = %d, want 404. %s", resp2.StatusCode, authorizeIsPermanently404)
	}
	if logins, callbacks := hub.counts(); logins != 1 || callbacks != 1 {
		t.Fatalf("hub saw %d logins / %d callbacks, want 1/1", logins, callbacks)
	}
}

// TestLoginRewritesTheHubIssuedRedirectOrigin: the hub builds redirect_uri
// from its own base_url (localhost:8080 by default), which is not an address
// the proxy can reach — it dials the hub through the jail. Only the origin may
// be replaced; the code stays bound to the redirect_uri as issued, because
// that is the string the hub re-sends to /token.
func TestLoginRewritesTheHubIssuedRedirectOrigin(t *testing.T) {
	d, hub, _, _ := newTestDriver(t)
	if strings.Contains(hub.URL, "8080") {
		t.Fatalf("test hub happens to serve the base_url address; the rewrite would not be exercised")
	}
	if _, err := d.Cookie(context.Background(), "op@example.test"); err != nil {
		t.Fatalf("Cookie: %v", err)
	}
	if _, callbacks := hub.counts(); callbacks != 1 {
		t.Fatalf("hub saw %d callbacks, want 1 — the callback must reach the hub as dialled", callbacks)
	}
}

func TestLoginRefusesAHubConfiguredAgainstAnotherProvider(t *testing.T) {
	d, hub, _, _ := newTestDriver(t)
	hub.authzOverride = "https://accounts.google.example/o/oauth2/auth"
	_, err := d.Cookie(context.Background(), "op@example.test")
	if err == nil {
		t.Fatal("logged in against a provider that is not ours")
	}
	if !strings.Contains(err.Error(), "different OIDC provider") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoginRefusesAnUnexpectedClientID(t *testing.T) {
	d, hub, _, _ := newTestDriver(t)
	hub.clientIDOverride = "someone-elses-client"
	_, err := d.Cookie(context.Background(), "op@example.test")
	if err == nil {
		t.Fatal("minted a code for another relying party's client_id")
	}
	if !strings.Contains(err.Error(), "client_id") {
		t.Fatalf("error = %v", err)
	}
}

// TestLoginReportsAStateMismatch models the jar being dropped between the
// login and the callback: scion stores the state in the very cookie that
// becomes the session, so the two steps must share one jar.
func TestLoginReportsAStateMismatch(t *testing.T) {
	d, hub, _, _ := newTestDriver(t)
	hub.dropState = true
	_, err := d.Cookie(context.Background(), "op@example.test")
	if err == nil {
		t.Fatal("login succeeded with no state on the hub side")
	}
	if !strings.Contains(err.Error(), "state_mismatch") {
		t.Fatalf("error = %v, want the hub's own reason", err)
	}
}

// TestConcurrentCallersShareOneLogin: a browser opens several connections at
// once and they all arrive before any session exists.
func TestConcurrentCallersShareOneLogin(t *testing.T) {
	d, hub, _, _ := newTestDriver(t)
	const callers = 12
	var wg sync.WaitGroup
	cookies := make([]string, callers)
	errs := make([]error, callers)
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			cookies[i], errs[i] = d.Cookie(context.Background(), "op@example.test")
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if cookies[i] != cookies[0] {
			t.Fatalf("caller %d got a different session", i)
		}
	}
	if logins, _ := hub.counts(); logins != 1 {
		t.Fatalf("hub saw %d logins for one page load, want 1", logins)
	}
}

func TestFailedLoginIsNotCached(t *testing.T) {
	d, hub, _, _ := newTestDriver(t)
	hub.dropState = true
	if _, err := d.Cookie(context.Background(), "op@example.test"); err == nil {
		t.Fatal("want an error")
	}
	hub.dropState = false
	if _, err := d.Cookie(context.Background(), "op@example.test"); err != nil {
		t.Fatalf("second attempt after a transient failure: %v", err)
	}
}

func TestInvalidateForcesAFreshLogin(t *testing.T) {
	d, hub, _, _ := newTestDriver(t)
	ctx := context.Background()
	first, err := d.Cookie(ctx, "op@example.test")
	if err != nil {
		t.Fatalf("Cookie: %v", err)
	}
	again, err := d.Cookie(ctx, "op@example.test")
	if err != nil || again != first {
		t.Fatalf("second call = %q/%v, want the cached session", again, err)
	}
	if logins, _ := hub.counts(); logins != 1 {
		t.Fatalf("hub saw %d logins, want 1 (the session must be cached)", logins)
	}

	// A stale cookie value must not discard a session someone else renewed.
	d.Invalidate("op@example.test", "some-older-cookie")
	if c, _ := d.Cookie(ctx, "op@example.test"); c != first {
		t.Fatal("invalidating a stale cookie value dropped the live session")
	}

	d.Invalidate("op@example.test", first)
	third, err := d.Cookie(ctx, "op@example.test")
	if err != nil {
		t.Fatalf("Cookie after invalidate: %v", err)
	}
	if third == first {
		t.Fatal("invalidate did not force a new session")
	}
	if logins, _ := hub.counts(); logins != 2 {
		t.Fatalf("hub saw %d logins, want 2", logins)
	}
}

// TestDistinctOperatorsGetDistinctSessions: the identity asserted at
// /userinfo is the tailnet login the proxy verified, so two operators end up
// as two hub users rather than sharing one.
func TestDistinctOperatorsGetDistinctSessions(t *testing.T) {
	d, hub, _, _ := newTestDriver(t)
	ctx := context.Background()
	a, err := d.Cookie(ctx, "alice@example.test")
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	b, err := d.Cookie(ctx, "bob@example.test")
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	if a == b {
		t.Fatal("two operators shared one hub session")
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.sessions[a] != "alice@example.test" || hub.sessions[b] != "bob@example.test" {
		t.Fatalf("hub recorded %q and %q", hub.sessions[a], hub.sessions[b])
	}
}

func TestIdentityForAnUnnamedOperator(t *testing.T) {
	id := identityFor("")
	if id.Email != unnamedOperatorEmail || id.Subject != unnamedOperator {
		t.Fatalf("identityFor(\"\") = %+v", id)
	}
	named := identityFor("op@example.test")
	if named.Email != "op@example.test" {
		t.Fatalf("identityFor(login).Email = %q, want the verified login", named.Email)
	}
}

func TestLoginAuditNeverCarriesTheSession(t *testing.T) {
	d, hub, _, psink := newTestDriver(t)
	sink := &auditSink{}
	d.audit = sink.add
	cookie, err := d.Cookie(context.Background(), "op@example.test")
	if err != nil {
		t.Fatalf("Cookie: %v", err)
	}
	hub.mu.Lock()
	stateCount := len(hub.states)
	hub.mu.Unlock()
	if stateCount == 0 {
		t.Fatal("test bug: the hub recorded no state")
	}
	assertNoSecretsInAudit(t, sink.all(), map[string]string{"session cookie": cookie})
	assertNoSecretsInAudit(t, psink.all(), map[string]string{"session cookie": cookie})

	// A failure is audited too, and its message is lever's own wording.
	hub.dropState = true
	d.Invalidate("op@example.test", cookie)
	if _, err := d.Cookie(context.Background(), "op@example.test"); err == nil {
		t.Fatal("want a failure to audit")
	}
	var failures int
	for _, l := range sink.all() {
		if l.Decision == "oidc-session-failed" {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("%d failure lines, want 1", failures)
	}
}

// TestALoginSurvivesTheRequestThatStartedIt: one browser request wins the race
// to log in and the others wait on it, so a client that navigates away mid-
// handshake must not take the login out from under everyone still waiting.
func TestALoginSurvivesTheRequestThatStartedIt(t *testing.T) {
	d, hub, _, _ := newTestDriver(t)
	release := make(chan struct{})
	hub.beforeCallback = func() { <-release }

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		cookie string
		err    error
	}
	first := make(chan result, 1)
	go func() {
		c, err := d.Cookie(ctx, "op@example.test")
		first <- result{c, err}
	}()
	// Let the handshake reach the callback, then abandon the request that
	// started it.
	time.Sleep(50 * time.Millisecond)
	cancel()
	close(release)

	if r := <-first; r.err != nil || r.cookie == "" {
		t.Fatalf("the login was abandoned with its request: %+v", r)
	}
	if c, err := d.Cookie(context.Background(), "op@example.test"); err != nil || c == "" {
		t.Fatalf("no usable session afterwards: %q %v", c, err)
	}
	if logins, _ := hub.counts(); logins != 1 {
		t.Fatalf("hub saw %d logins, want 1", logins)
	}
}

// mintedCodes reads the codes the provider currently holds. Used by the
// hygiene test below, which needs the one secret the audit log must never
// contain and which no API exposes on purpose.
func mintedCodes(p *Provider) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.codes))
	for code := range p.codes {
		out = append(out, code)
	}
	return out
}

// TestAFailedCallbackNeverAuditsTheCode is the regression test for a real
// leak: http.Client.Do returns a *url.Error whose message quotes the WHOLE
// request URL, and the callback leg carries the authorization code in its
// query. A wedged jail dial, a hub that dies mid-handshake, or the login
// timeout was enough to write a live code into the audit log and remote.log.
func TestAFailedCallbackNeverAuditsTheCode(t *testing.T) {
	d, hub, p, _ := newTestDriver(t)
	sink := &auditSink{}
	d.audit = sink.add
	// Die at the callback the way a dropped jail transport does: no status, no
	// body, just a closed connection.
	hub.beforeCallback = func() { panic(http.ErrAbortHandler) }

	_, err := d.Cookie(context.Background(), "op@example.test")
	if err == nil {
		t.Fatal("want the callback to fail")
	}
	codes := mintedCodes(p)
	if len(codes) != 1 {
		t.Fatalf("%d codes held by the provider, want the 1 that was minted for this attempt", len(codes))
	}
	code := codes[0]
	if strings.Contains(err.Error(), code) {
		t.Fatalf("the returned error quotes the authorization code: %v", err)
	}
	assertNoSecretsInAudit(t, sink.all(), map[string]string{"authorization code": code})
	// The operator still has to be able to tell WHICH leg failed.
	if !strings.Contains(err.Error(), callbackPathPrefix) {
		t.Fatalf("error names no path, so the failing leg is unidentifiable: %v", err)
	}
}

// TestALoginNeedsANewCookieNotTheStateOne: scion_sess carries the login state
// before it carries the session, so "the jar has a scion_sess" is not proof of
// a login. A hub that answers without replacing it must not look like success.
func TestALoginNeedsANewCookieNotTheStateOne(t *testing.T) {
	d, hub, _, _ := newTestDriver(t)
	hub.callbackFails = true

	_, err := d.Cookie(context.Background(), "op@example.test")
	if err == nil {
		t.Fatal("the login-state cookie was accepted as a session")
	}
	if !strings.Contains(err.Error(), "state") {
		t.Fatalf("error = %v, want it to name the unreplaced login-state cookie", err)
	}
}
