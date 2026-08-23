package remoteproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// testIdentity is the operator the provider asserts in these tests.
var testIdentity = Identity{Subject: "lever-remote:op@example.test", Email: "op@example.test", Name: "op@example.test"}

// newTestProvider returns a provider plus the audit lines it emits.
func newTestProvider(t *testing.T, now func() time.Time) (*Provider, *auditSink) {
	t.Helper()
	sink := &auditSink{}
	p := NewProvider(ProviderConfig{Port: 8446, Audit: sink.add})
	if now != nil {
		p.now = now // the in-package clock seam; nil keeps the real clock
	}
	return p, sink
}

// auditSink collects audit lines for the assertions that nothing secret ever
// reaches one.
type auditSink struct {
	mu    sync.Mutex
	lines []AuditLine
}

func (s *auditSink) add(l AuditLine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, l)
}

func (s *auditSink) all() []AuditLine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AuditLine(nil), s.lines...)
}

// serveProvider runs a request against the provider's handler.
func serveProvider(p *Provider, req *http.Request) *httptest.ResponseRecorder {
	rw := httptest.NewRecorder()
	p.Handler().ServeHTTP(rw, req)
	return rw
}

// postToken exchanges a code at /token with the parameters the hub sends.
func postToken(p *Provider, code, grantType, redirectURI, clientID string) *httptest.ResponseRecorder {
	form := url.Values{"grant_type": {grantType}, "code": {code}, "redirect_uri": {redirectURI}, "client_id": {clientID}}
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return serveProvider(p, req)
}

const (
	testRedirectURI = "http://localhost:8080/auth/callback/oidc"
	testState       = "0123456789abcdef0123456789abcdef"
)

// mintTestCode mints a code for the standard test login attempt.
func mintTestCode(t *testing.T, p *Provider) string {
	t.Helper()
	code, err := p.Mint(testState, testRedirectURI, LoginClientID, testIdentity)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return code
}

// TestAuthorizeIsPermanently404 pins the decision recorded in
// Provider.handleAuthorize. If this test fails because /authorize now answers,
// the failure is not a broken test: an authorization endpoint is an HTTP
// endpoint that MINTS AUTHORIZATION CODES, this provider is reachable from
// inside the jail (lever maps guest loopback into every agent netns at
// 169.254.1.2), and scion's OIDC login validates no signature — so any jailed
// agent could mint a code, have the hub exchange it, and receive a hub web
// session as any identity it asserts. Do not "finish" this route. The login is
// driven server-side and mints codes by calling Provider.Mint directly.
// authorizeIsPermanently404 names the decision handleAuthorize enforces, so
// the grep that finds "/authorize" in this package also finds the reason it
// 404s.
const authorizeIsPermanently404 = "/authorize must never be implemented: it would be an HTTP code-minting endpoint reachable from inside the jail"

func TestAuthorizeIsPermanently404(t *testing.T) {
	p, sink := newTestProvider(t, nil)
	for _, target := range []string{
		"/authorize",
		"/authorize?client_id=" + LoginClientID + "&redirect_uri=" + url.QueryEscape(testRedirectURI) + "&response_type=code&state=abc",
	} {
		rw := serveProvider(p, httptest.NewRequest(http.MethodGet, target, nil))
		if rw.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404. %s", target, rw.Code, authorizeIsPermanently404)
		}
	}
	// The hit must be visible: nothing legitimate calls this route, so a hit
	// is a misconfiguration or an in-jail probe.
	var audited int
	for _, l := range sink.all() {
		if l.Decision == "deny-authorize" {
			audited++
			if l.Status != http.StatusNotFound {
				t.Fatalf("audited status = %d, want 404", l.Status)
			}
		}
	}
	if audited != 2 {
		t.Fatalf("audited %d /authorize hits, want 2 — every hit must be recorded", audited)
	}
}

// TestDiscoveryAdvertisesAnEndpointThisProviderDoesNotServe covers the other
// half of the same decision: scion refuses to start a login unless discovery
// carries an authorization_endpoint, so one is advertised — but it must point
// at a host that cannot resolve, never at this provider.
func TestDiscoveryAdvertisesAnEndpointThisProviderDoesNotServe(t *testing.T) {
	p, _ := newTestProvider(t, nil)
	rw := serveProvider(p, httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("discovery = %d, want 200", rw.Code)
	}
	var doc map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &doc); err != nil {
		t.Fatalf("discovery body: %v", err)
	}
	authz, _ := doc["authorization_endpoint"].(string)
	if authz != DeadAuthorizationEndpoint {
		t.Fatalf("authorization_endpoint = %q, want %q", authz, DeadAuthorizationEndpoint)
	}
	if strings.HasPrefix(authz, p.IssuerURL()) {
		t.Fatalf("authorization_endpoint %q points at this provider. %s", authz, authorizeIsPermanently404)
	}
	if got := doc["token_endpoint"]; got != p.IssuerURL()+"/token" {
		t.Fatalf("token_endpoint = %v", got)
	}
	if got := doc["userinfo_endpoint"]; got != p.IssuerURL()+"/userinfo" {
		t.Fatalf("userinfo_endpoint = %v", got)
	}
	// jwks_uri is deliberately absent: no id_token is ever issued, so the hub
	// never fetches keys, and advertising a URI nothing serves would be a lie.
	if _, ok := doc["jwks_uri"]; ok {
		t.Fatalf("discovery advertises jwks_uri, which nothing serves")
	}
}

func TestMintProducesUnguessableSingleUseCodes(t *testing.T) {
	p, _ := newTestProvider(t, nil)
	seen := map[string]bool{}
	for range 100 {
		code := mintTestCode(t, p)
		if len(code) != 2*secretBytes {
			t.Fatalf("code length %d, want %d hex chars", len(code), 2*secretBytes)
		}
		if seen[code] {
			t.Fatalf("Mint repeated a code")
		}
		seen[code] = true
	}
}

func TestTokenRejectsAGuessedCode(t *testing.T) {
	p, _ := newTestProvider(t, nil)
	mintTestCode(t, p) // a real code exists; the guess still must not work
	for _, guess := range []string{"", "guessed-code", strings.Repeat("a", 2*secretBytes)} {
		rw := postToken(p, guess, "authorization_code", testRedirectURI, LoginClientID)
		if rw.Code != http.StatusBadRequest {
			t.Fatalf("guessed code %q = %d, want 400", guess, rw.Code)
		}
		if !strings.Contains(rw.Body.String(), "invalid_grant") {
			t.Fatalf("guessed code %q body = %s", guess, rw.Body.String())
		}
	}
}

func TestTokenIsSingleUse(t *testing.T) {
	p, _ := newTestProvider(t, nil)
	code := mintTestCode(t, p)
	if rw := postToken(p, code, "authorization_code", testRedirectURI, LoginClientID); rw.Code != http.StatusOK {
		t.Fatalf("first exchange = %d, want 200", rw.Code)
	}
	rw := postToken(p, code, "authorization_code", testRedirectURI, LoginClientID)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("replayed code = %d, want 400", rw.Code)
	}
}

// TestTokenConsumesTheCodeEvenWhenABindingFails: a code shown to /token is
// spent whatever the outcome, so an attacker who somehow held one could not
// probe the other parameters by retrying.
func TestTokenConsumesTheCodeEvenWhenABindingFails(t *testing.T) {
	p, _ := newTestProvider(t, nil)
	code := mintTestCode(t, p)
	if rw := postToken(p, code, "authorization_code", "https://evil.example/cb", LoginClientID); rw.Code != http.StatusBadRequest {
		t.Fatalf("mismatched redirect_uri = %d, want 400", rw.Code)
	}
	if rw := postToken(p, code, "authorization_code", testRedirectURI, LoginClientID); rw.Code != http.StatusBadRequest {
		t.Fatalf("correct parameters after a failed attempt = %d, want 400 (the code must already be spent)", rw.Code)
	}
}

func TestTokenRejectsExpiredCode(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	p, _ := newTestProvider(t, clock)
	code := mintTestCode(t, p)
	now = now.Add(codeTTL + time.Second)
	rw := postToken(p, code, "authorization_code", testRedirectURI, LoginClientID)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("expired code = %d, want 400", rw.Code)
	}
}

func TestTokenRejectsEachMismatchedBinding(t *testing.T) {
	cases := []struct {
		name                             string
		grantType, redirectURI, clientID string
	}{
		{"redirect_uri", "authorization_code", "http://localhost:8080/auth/callback/evil", LoginClientID},
		{"client_id", "authorization_code", testRedirectURI, "someone-else"},
		{"grant_type", "refresh_token", testRedirectURI, LoginClientID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newTestProvider(t, nil)
			code := mintTestCode(t, p)
			rw := postToken(p, code, tc.grantType, tc.redirectURI, tc.clientID)
			if rw.Code != http.StatusBadRequest {
				t.Fatalf("mismatched %s = %d, want 400", tc.name, rw.Code)
			}
		})
	}
}

func TestTokenRefusesGET(t *testing.T) {
	p, _ := newTestProvider(t, nil)
	code := mintTestCode(t, p)
	rw := serveProvider(p, httptest.NewRequest(http.MethodGet, "/token?code="+code, nil))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /token = %d, want 405", rw.Code)
	}
}

// TestCallbackQueryEnforcesTheStateBinding covers the third binding. The hub
// never sends state to /token, so this is the only place a code paired with
// the wrong login attempt can be caught.
func TestCallbackQueryEnforcesTheStateBinding(t *testing.T) {
	p, _ := newTestProvider(t, nil)
	code := mintTestCode(t, p)
	if _, err := p.CallbackQuery(code, testState+"x"); err == nil {
		t.Fatal("CallbackQuery accepted a state the code was not minted for")
	}
	if _, err := p.CallbackQuery(code, ""); err == nil {
		t.Fatal("CallbackQuery accepted an empty state")
	}
	if _, err := p.CallbackQuery("no-such-code", testState); err == nil {
		t.Fatal("CallbackQuery accepted an unknown code")
	}
	q, err := p.CallbackQuery(code, testState)
	if err != nil {
		t.Fatalf("CallbackQuery with the right state: %v", err)
	}
	if q.Get("code") != code || q.Get("state") != testState {
		t.Fatalf("callback query = %v", q)
	}
}

func TestUserinfoNeedsTheAccessTokenAndAssertsTheVerifiedOperator(t *testing.T) {
	p, _ := newTestProvider(t, nil)
	code := mintTestCode(t, p)

	for _, header := range []string{"", "Bearer", "Bearer wrong-token", "Basic abc"} {
		req := httptest.NewRequest(http.MethodGet, "/userinfo", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		if rw := serveProvider(p, req); rw.Code != http.StatusUnauthorized {
			t.Fatalf("userinfo with %q = %d, want 401", header, rw.Code)
		}
	}

	tokenRW := postToken(p, code, "authorization_code", testRedirectURI, LoginClientID)
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(tokenRW.Body.Bytes(), &tok); err != nil {
		t.Fatalf("token body: %v", err)
	}
	if tok.AccessToken == "" || tok.TokenType != "Bearer" {
		t.Fatalf("token response = %s", tokenRW.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/userinfo", nil)
	req.Header.Set("Authorization", "bearer "+tok.AccessToken) // scheme match is case-insensitive
	rw := serveProvider(p, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("userinfo = %d, want 200", rw.Code)
	}
	var info map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &info); err != nil {
		t.Fatalf("userinfo body: %v", err)
	}
	// scion refuses a login whose email is unverified, requires a sub, and
	// keys its user row on the email.
	if info["sub"] != testIdentity.Subject || info["email"] != testIdentity.Email || info["name"] != testIdentity.Name {
		t.Fatalf("userinfo = %v, want the verified operator", info)
	}
	if info["email_verified"] != true {
		t.Fatalf("email_verified = %v, want true (scion refuses the login without it)", info["email_verified"])
	}
}

func TestUserinfoRejectsAnExpiredToken(t *testing.T) {
	now := time.Now()
	p, _ := newTestProvider(t, func() time.Time { return now })
	code := mintTestCode(t, p)
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(postToken(p, code, "authorization_code", testRedirectURI, LoginClientID).Body.Bytes(), &tok); err != nil {
		t.Fatalf("token body: %v", err)
	}
	now = now.Add(tokenTTL + time.Second)
	req := httptest.NewRequest(http.MethodGet, "/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	if rw := serveProvider(p, req); rw.Code != http.StatusUnauthorized {
		t.Fatalf("expired token = %d, want 401", rw.Code)
	}
}

// TestProviderSweepsSpentGrants keeps the two maps from growing with every
// login for the life of a long-running proxy.
func TestProviderSweepsSpentGrants(t *testing.T) {
	now := time.Now()
	p, _ := newTestProvider(t, func() time.Time { return now })
	for range 20 {
		mintTestCode(t, p)
	}
	now = now.Add(codeTTL + time.Second)
	mintTestCode(t, p) // any mint sweeps
	p.mu.Lock()
	live := len(p.codes)
	p.mu.Unlock()
	if live != 1 {
		t.Fatalf("%d codes retained, want 1 (expired grants must be swept)", live)
	}
}

func TestUnknownPathsAre404AndAudited(t *testing.T) {
	p, sink := newTestProvider(t, nil)
	for _, path := range []string{"/mint", "/", "/token/", "/.well-known/"} {
		if rw := serveProvider(p, httptest.NewRequest(http.MethodGet, path, nil)); rw.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, rw.Code)
		}
	}
	if n := len(sink.all()); n != 4 {
		t.Fatalf("%d audit lines, want 4 (every probe is recorded)", n)
	}
}

// TestAuditPathIsBounded: anything in the jail can probe this provider, and
// the path is attacker-chosen text landing in the operator's log.
func TestAuditPathIsBounded(t *testing.T) {
	p, sink := newTestProvider(t, nil)
	serveProvider(p, httptest.NewRequest(http.MethodGet, "/"+strings.Repeat("z", 4000), nil))
	for _, l := range sink.all() {
		if len(l.Path) > maxAuditFieldLen+len("…") {
			t.Fatalf("audited path length %d, want <= %d", len(l.Path), maxAuditFieldLen)
		}
	}
}

// TestProviderAuditNeverCarriesASecret drives a whole exchange and checks that
// no audit line quotes the code or the access token.
func TestProviderAuditNeverCarriesASecret(t *testing.T) {
	p, sink := newTestProvider(t, nil)
	code := mintTestCode(t, p)
	rw := postToken(p, code, "authorization_code", testRedirectURI, LoginClientID)
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &tok); err != nil {
		t.Fatalf("token body: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	serveProvider(p, req)
	// A refusal quotes a reason, never the value that was refused.
	postToken(p, code, "authorization_code", testRedirectURI, LoginClientID)

	assertNoSecretsInAudit(t, sink.all(), map[string]string{"code": code, "access token": tok.AccessToken})
}

// assertNoSecretsInAudit serialises every line the way the audit file does and
// fails if any secret appears anywhere in it.
func assertNoSecretsInAudit(t *testing.T, lines []AuditLine, secrets map[string]string) {
	t.Helper()
	if len(lines) == 0 {
		t.Fatal("no audit lines were emitted")
	}
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatalf("marshal audit line: %v", err)
		}
		for name, secret := range secrets {
			if secret == "" {
				t.Fatalf("test bug: empty %s", name)
			}
			if strings.Contains(string(b), secret) {
				t.Fatalf("audit line leaks the %s: %s", name, b)
			}
		}
	}
}
