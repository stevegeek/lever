package remoteproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
)

// startProvider serves a Provider on a real loopback listener whose port it
// knows, so discovery advertises endpoints that resolve.
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

// fakeScionHub models the parts of scion's web layer the handshake touches:
// one scion_sess cookie carrying state then session; a redirect_uri built
// from the hub's own unreachable base_url; a callback that compares state and
// calls /token and /userinfo itself; and a shell/API that need a session
// unless the request carries an Authorization header.
type fakeScionHub struct {
	*httptest.Server
	issuer   string // the configured oidc_login.issuer_url
	clientID string

	// baseURL mirrors scion's default http://localhost:<web-port>, which the
	// proxy cannot reach.
	baseURL string

	mu        sync.Mutex
	states    map[string]string // state cookie value -> state
	sessions  map[string]string // session cookie value -> email
	logins    int
	callbacks int
	apiCalls  []http.Header // headers of every /api/v1 request

	dropState        bool   // "forget" the state, as a lost cookie jar would
	authzOverride    string // redirect somewhere other than our provider
	clientIDOverride string // ask for a client_id lever did not configure
	rejectSessions   bool   // treat every session as unknown (a restarted hub)
	whoAmIID         string // hub user id /api/v1/auth/me reports (default testHubUserID)
	whoAmIStatus     int    // non-zero: /api/v1/auth/me answers this status instead
	beforeCallback   func() // runs at the top of the callback, to hold a handshake open
	callbackFails    bool   // answer the callback 500, leaving the state cookie in place
}

func newFakeScionHub(t *testing.T, issuer string) *fakeScionHub {
	t.Helper()
	h := &fakeScionHub{
		issuer:   issuer,
		clientID: LoginClientID,
		baseURL:  "http://localhost:8080",
		states:   map[string]string{},
		sessions: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login/oidc", h.handleLogin)
	mux.HandleFunc("/auth/callback/oidc", h.handleCallback)
	// A dedicated handler so the login driver's post-login whoAmI resolves a
	// valid, non-dev hub user id. Longest-match wins over /api/v1/, and it is
	// not counted in apiCalls (the gate never forwards this path).
	mux.HandleFunc("/api/v1/auth/me", h.handleWhoAmI)
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
		// Neither redirect nor session: the jar keeps the login STATE cookie.
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

// exchange re-sends the SAME redirect_uri it issued, which binds the code.
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

// testHubUserID is the non-dev hub user id the fake hub reports for every
// signed-in session; the login driver's dev-user rule must let it through.
const testHubUserID = "11111111-2222-4333-8444-555555555555"

func (h *fakeScionHub) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	email, ok := h.sessionEmail(r)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if h.whoAmIStatus != 0 {
		w.WriteHeader(h.whoAmIStatus)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	id := h.whoAmIID
	if id == "" {
		id = testHubUserID
	}
	_, _ = fmt.Fprintf(w, `{"id":%q,"email":%q}`, id, email)
}

func (h *fakeScionHub) handleShell(w http.ResponseWriter, r *http.Request) {
	if email, ok := h.sessionEmail(r); ok {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "shell for %s", email)
		return
	}
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

// handleAPI models sessionToBearerMiddleware: a request with an Authorization
// header passes through to the hub's own auth (where the narrow PAT is checked).
func (h *fakeScionHub) handleAPI(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.apiCalls = append(h.apiCalls, r.Header.Clone())
	h.mu.Unlock()
	if r.Header.Get("Authorization") != "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if email, ok := h.sessionEmail(r); ok {
		_, _ = fmt.Fprintf(w, `{"ok":true,"via":"session","user":%q}`, email)
		return
	}
	w.WriteHeader(http.StatusUnauthorized)
}

func (h *fakeScionHub) counts() (logins, callbacks int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logins, h.callbacks
}

func newTestDriver(t *testing.T) (*LoginDriver, *fakeScionHub, *Provider, *auditSink) {
	t.Helper()
	p, psink, _ := startProvider(t)
	hub := newFakeScionHub(t, p.IssuerURL())
	sink := &auditSink{}
	d := NewLoginDriver(LoginConfig{Hub: mustURL(t, hub.URL), Provider: p, Audit: sink.add})
	return d, hub, p, psink
}

func TestHandshakeCompletesWithNoAuthorizeRouteRegistered(t *testing.T) {
	d, hub, p, psink := newTestDriver(t)

	cookie, err := d.Cookie(context.Background(), "op@example.test")
	if err != nil {
		t.Fatalf("Cookie: %v", err)
	}
	if cookie == "" {
		t.Fatal("handshake returned an empty session")
	}

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

	// The provider answered only the three hub endpoints. /authorize here
	// means a code-minting endpoint reachable from the jail.
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

// The hub builds redirect_uri from its unreachable base_url: only the origin
// may change, since the code is bound to the redirect_uri as issued.
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
	if !errors.Is(err, errForeignProvider) {
		t.Fatalf("error = %v, want errForeignProvider", err)
	}
}

func TestLoginRefusesAnUnexpectedClientID(t *testing.T) {
	d, hub, _, _ := newTestDriver(t)
	hub.clientIDOverride = "someone-elses-client"
	_, err := d.Cookie(context.Background(), "op@example.test")
	if err == nil {
		t.Fatal("minted a code for another relying party's client_id")
	}
	if !errors.Is(err, errForeignClientID) {
		t.Fatalf("error = %v, want errForeignClientID", err)
	}
}

// TestLoginRefusesASessionThatResolvesToTheDevUser is the core mitigation: an
// agent renamed scion's dev super-admin row to this login's email, so the
// session lands on the super-admin row. The driver must refuse it.
func TestLoginRefusesASessionThatResolvesToTheDevUser(t *testing.T) {
	for _, login := range []string{"op@example.test", "contact-id", ""} {
		t.Run("login="+login, func(t *testing.T) {
			d, hub, _, _ := newTestDriver(t)
			clock := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
			d.now = func() time.Time { return clock }
			hub.whoAmIID = config.ScionDevUserID
			if _, err := d.Cookie(context.Background(), login); !errors.Is(err, errForeignHubUser) {
				t.Fatalf("error = %v, want errForeignHubUser", err)
			}
			// Within the backoff the refusal is answered without another sign-in,
			// even once the hub would now resolve a different user.
			hub.whoAmIID = ""
			logins, _ := hub.counts()
			if _, err := d.Cookie(context.Background(), login); !errors.Is(err, errForeignHubUser) {
				t.Fatalf("within the backoff: %v, want the cached refusal", err)
			}
			if again, _ := hub.counts(); again != logins {
				t.Fatalf("a refused login signed in again within the backoff (%d -> %d)", logins, again)
			}
			clock = clock.Add(refusalBackoff)
			if _, err := d.Cookie(context.Background(), login); err != nil {
				t.Fatalf("after the backoff: %v", err)
			}
		})
	}
}

// TestLoginFailsClosedWhenTheUserCannotBeRead: no answer from /auth/me is
// never read as "allowed".
func TestLoginFailsClosedWhenTheUserCannotBeRead(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusFound, http.StatusUnauthorized} {
		d, hub, _, _ := newTestDriver(t)
		hub.whoAmIStatus = status
		if _, err := d.Cookie(context.Background(), "op@example.test"); err == nil {
			t.Fatalf("/auth/me %d: login succeeded", status)
		}
	}
}

// TestLoginRefusesTheDevUserEvenWhenRecorded: a record that names the dev id
// (written before apply refused it) does not make the dev row acceptable.
func TestLoginRefusesTheDevUserEvenWhenRecorded(t *testing.T) {
	p, _, _ := startProvider(t)
	hub := newFakeScionHub(t, p.IssuerURL())
	hub.whoAmIID = config.ScionDevUserID
	d := NewLoginDriver(LoginConfig{Hub: mustURL(t, hub.URL), Provider: p,
		BoundUser: func(string) (string, bool) { return config.ScionDevUserID, true }})
	if _, err := d.Cookie(context.Background(), "op@example.test"); !errors.Is(err, errForeignHubUser) {
		t.Fatalf("error = %v, want errForeignHubUser", err)
	}
}

// TestLoginRefusesASessionWhoseIDIsNotTheBoundOne: lever recorded a hub user
// id for the login, and the session resolves to a different one (a swapped or
// re-provisioned row).
func TestLoginRefusesASessionWhoseIDIsNotTheBoundOne(t *testing.T) {
	p, _, _ := startProvider(t)
	hub := newFakeScionHub(t, p.IssuerURL())
	d := NewLoginDriver(LoginConfig{Hub: mustURL(t, hub.URL), Provider: p,
		BoundUser: func(string) (string, bool) { return "a-different-id", true }})
	if _, err := d.Cookie(context.Background(), "op@example.test"); !errors.Is(err, errForeignHubUser) {
		t.Fatalf("error = %v, want errForeignHubUser", err)
	}
}

// TestLoginAcceptsTheBoundID: the session resolves to exactly the id lever
// recorded.
func TestLoginAcceptsTheBoundID(t *testing.T) {
	p, _, _ := startProvider(t)
	hub := newFakeScionHub(t, p.IssuerURL())
	d := NewLoginDriver(LoginConfig{Hub: mustURL(t, hub.URL), Provider: p,
		BoundUser: func(string) (string, bool) { return testHubUserID, true }})
	if _, err := d.Cookie(context.Background(), "op@example.test"); err != nil {
		t.Fatalf("a session matching the bound id was refused: %v", err)
	}
}

// TestLoginReportsAStateMismatch models a jar dropped between login and
// callback: scion keeps the state in the cookie that becomes the session.
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

// scion refuses a scion_sess older than 24 hours and a refused POST cannot be
// retried, so the driver renews at sessionMaxAge.
func TestSessionIsRenewedBeforeTheHubRefusesIt(t *testing.T) {
	d, hub, _, _ := newTestDriver(t)
	clock := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }
	ctx := context.Background()
	first, err := d.Cookie(ctx, "op@example.test")
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(sessionMaxAge - time.Minute)
	if c, _ := d.Cookie(ctx, "op@example.test"); c != first {
		t.Fatal("a session younger than sessionMaxAge was replaced")
	}
	clock = clock.Add(time.Minute)
	renewed, err := d.Cookie(ctx, "op@example.test")
	if err != nil || renewed == first {
		t.Fatalf("at sessionMaxAge: %q/%v, want a fresh session", renewed, err)
	}
	if logins, _ := hub.counts(); logins != 2 {
		t.Fatalf("hub saw %d logins, want 2", logins)
	}
	if sessionMaxAge >= 24*time.Hour {
		t.Fatalf("sessionMaxAge %v must stay under scion's 24h cookie MaxAge", sessionMaxAge)
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

// Two operators must become two hub users, not share one.
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

// The emails the bootstrap step binds must be the ones sign-in creates.
func TestHubUserEmailsMatchIdentityFor(t *testing.T) {
	if got := HubUserEmails(nil); len(got) != 1 || got[0] != unnamedOperatorEmail {
		t.Fatalf("HubUserEmails(nil) = %v, want the placeholder", got)
	}
	got := HubUserEmails([]string{"a@github", "b@github"})
	if len(got) != 2 || got[0] != identityFor("a@github").Email || got[1] != identityFor("b@github").Email {
		t.Fatalf("HubUserEmails = %v", got)
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

// A client that navigates away mid-handshake must not cancel the login others
// are waiting on.
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

// mintedCodes reads the provider's codes: the secret the audit log must never
// contain.
func mintedCodes(p *Provider) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.codes))
	for code := range p.codes {
		out = append(out, code)
	}
	return out
}

// A *url.Error quotes the whole URL, and the callback's query carries the
// code: a failed callback must not put it in the audit log.
func TestAFailedCallbackNeverAuditsTheCode(t *testing.T) {
	d, hub, p, _ := newTestDriver(t)
	sink := &auditSink{}
	d.audit = sink.add
	// Die at the callback like a dropped jail transport: a closed connection.
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

// scion_sess holds the login state before the session, so a hub that answers
// without replacing it must not look like success.
func TestALoginNeedsANewCookieNotTheStateOne(t *testing.T) {
	d, hub, _, _ := newTestDriver(t)
	hub.callbackFails = true

	_, err := d.Cookie(context.Background(), "op@example.test")
	if err == nil {
		t.Fatal("the login-state cookie was accepted as a session")
	}
	if !errors.Is(err, errNoSessionMinted) {
		t.Fatalf("error = %v, want errNoSessionMinted", err)
	}
}

// net/http recovers a handler panic, so the single-flight entry must still be
// released or every later request for that login waits forever. The panic is
// injected through the audit sink.
func TestAPanickingAttemptDoesNotWedgeTheDriver(t *testing.T) {
	p, _, _ := startProvider(t)
	hub := newFakeScionHub(t, p.IssuerURL())
	var attempts atomic.Int32
	d := NewLoginDriver(LoginConfig{
		Hub:      mustURL(t, hub.URL),
		Provider: p,
		Audit: func(AuditLine) {
			if attempts.Add(1) == 1 {
				panic("audit sink exploded")
			}
		},
	})

	const operator = "op@example.test"
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("the panic was swallowed: net/http's own recovery, and the stack trace it logs, is what names the real fault")
			}
		}()
		_, _ = d.Cookie(context.Background(), operator)
	}()

	// Background context: if the panicked attempt is still in the map, this
	// never returns.
	type result struct {
		cookie string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		c, err := d.Cookie(context.Background(), operator)
		done <- result{c, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || r.cookie == "" {
			t.Fatalf("the login after a panicking one failed: %q %v", r.cookie, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second login for the same operator never returned: the panicked attempt left its single-flight entry unreleased")
	}
	if logins, _ := hub.counts(); logins != 2 {
		t.Fatalf("hub saw %d logins, want 2 (the panicked attempt, then a real retry)", logins)
	}
}

// Waiters on a panicked attempt must get an error: the zero cookie a panic
// leaves would otherwise read as an empty session.
func TestAPanickingAttemptReleasesTheWaitersOnIt(t *testing.T) {
	p, _, _ := startProvider(t)
	hub := newFakeScionHub(t, p.IssuerURL())
	reached, release := make(chan struct{}), make(chan struct{})
	hub.beforeCallback = func() { close(reached); <-release }
	d := NewLoginDriver(LoginConfig{
		Hub:      mustURL(t, hub.URL),
		Provider: p,
		Audit:    func(AuditLine) { panic("audit sink exploded") },
	})

	const operator = "op@example.test"
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		_, _ = d.Cookie(context.Background(), operator)
	}()

	<-reached
	d.mu.Lock()
	e := d.sessions[operator]
	d.mu.Unlock()
	if e == nil {
		t.Fatal("test bug: no in-flight entry while the handshake is held open")
	}
	close(release)
	if r := <-panicked; r == nil {
		t.Fatal("test bug: the sink did not panic, so nothing here was exercised")
	}

	select {
	case <-e.done:
	default:
		t.Fatal("the attempt's channel is still open: every caller waiting on it stays parked for the life of the process")
	}
	if e.err == nil {
		t.Fatal("a waiter would read the panicked attempt as a successful login carrying an empty session")
	}
}
