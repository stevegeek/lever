package remoteproxy

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The local OIDC provider.
//
// scion's web layer authenticates a browser by ONE thing: the `scion_sess`
// cookie (it never reads Authorization, and the API turns the cookie into a
// bearer), so that cookie is what the proxy sends on every request. The
// supported way to obtain it without dev-auth is scion's generic OIDC login,
// so lever runs the smallest provider that login path consumes.
//
// scion validates nothing on that path: no id_token, no JWKS, no PKCE, no
// nonce, no issuer check. It exchanges a code at /token and reads /userinfo.
// The security therefore does NOT come from OIDC but from ONE property: an
// authorization code can only be created by an in-process call to
// Provider.Mint inside the host-side proxy. No HTTP route mints one (see
// handleAuthorize).
//
// Discovery, /token and /userinfo are also reachable from inside the jail
// (the guest-side forwarder is reachable from every agent netns at
// 169.254.1.2; see internal/backend/guest.EnsureRuntimes). Without a code,
// none of them yields a token, a session or an identity.

const (
	// LoginClientID is the client_id lever writes into the hub's oidc_login
	// block (internal/backend/guest.EnsureHubLogin) and the only one this
	// provider mints a code for. With a single local relying party it is a
	// consistency check, not a credential; exported so the two ends cannot
	// drift.
	LoginClientID = "lever-remote"

	// codeTTL bounds how long a minted code may sit unredeemed, so a code
	// that never reaches the hub cannot be redeemed later.
	codeTTL = 60 * time.Second

	// tokenTTL bounds the access token /token hands back. The hub uses it
	// once, for /userinfo. NOT single-use: a retry inside the hub's exchange
	// would fail for no security gain, since the token grants nothing but
	// the identity lever already chose.
	tokenTTL = 2 * time.Minute

	// secretBytes is the entropy in a code and in an access token (from
	// crypto/rand): the code is the only secret in the whole flow.
	secretBytes = 32

	// DeadAuthorizationEndpoint is what discovery advertises for
	// authorization_endpoint. scion's discovery parse requires the field
	// (/auth/login/oidc 500s without it) but nothing dials it: the proxy
	// drives the login server-side. It names a host that cannot resolve and
	// deliberately does NOT point at this provider's /authorize (see
	// handleAuthorize). `lever doctor` asserts the hub redirects HERE, which
	// proves the hub is configured against lever's provider.
	DeadAuthorizationEndpoint = "https://lever.invalid/authorize"
)

// Identity is the operator the provider asserts at /userinfo: the login the
// proxy already verified against remote.allowed_users, never one this
// provider invents.
type Identity struct {
	Subject string // OIDC `sub`; stable per operator
	Email   string // the hub keys its user row on this
	Name    string // display name
}

type ProviderConfig struct {
	// Port is the loopback port the provider listens on, ON THE HOST.
	Port int
	// IssuerPort is the port the HUB dials: the guest-side loopback port the
	// forwarder listens on (the hub refuses a non-loopback http issuer). It
	// differs from Port because OrbStack mirrors a guest listener onto the
	// host at the same port (see config.GuestLoginIssuerPort). Zero means
	// "the same as Port", for tests with nothing in between.
	IssuerPort int
	// Audit receives one line per request the provider answers; nil disables
	// (tests). Values never carry a code, token or cookie.
	Audit func(line AuditLine)
}

// Provider is the host-side OIDC provider: three endpoints the hub consumes,
// plus a mint that is NOT one of them.
type Provider struct {
	port    int
	issuer  string
	audit   func(AuditLine)
	now     func() time.Time
	handler http.Handler

	mu     sync.Mutex
	codes  map[string]*grant
	tokens map[string]*grant
}

type grant struct {
	state       string
	redirectURI string
	clientID    string
	identity    Identity
	accessToken string
	expires     time.Time
}

// NewProvider builds the provider. It listens on nothing by itself; Serve
// binds the loopback listener (see ServeConfig.Provider).
func NewProvider(cfg ProviderConfig) *Provider {
	issuerPort := cfg.IssuerPort
	if issuerPort == 0 {
		issuerPort = cfg.Port
	}
	p := &Provider{
		port:   cfg.Port,
		issuer: fmt.Sprintf("http://127.0.0.1:%d", issuerPort),
		audit:  cfg.Audit,
		now:    time.Now, // tests swap the clock in-package to drive expiry
		codes:  map[string]*grant{},
		tokens: map[string]*grant{},
	}
	p.handler = p.buildHandler()
	return p
}

// Port is the HOST loopback port Serve binds this provider on. It is not the
// port the hub dials — see IssuerURL.
func (p *Provider) Port() int { return p.port }

// IssuerURL is the value that goes into the hub's oidc_login.issuer_url and
// the prefix of every endpoint discovery advertises. It names the GUEST-side
// port, because the hub is what dials it: the guest forwarder listening there
// carries the request to this process on the host.
func (p *Provider) IssuerURL() string { return p.issuer }

// Handler is the provider's HTTP surface: discovery, /token, /userinfo, the
// permanent /authorize 404, and a 404 catch-all.
func (p *Provider) Handler() http.Handler { return p.handler }

// Mint creates an authorization code bound to one login attempt. This is
// the whole authorization step, and it is a function call: only the proxy
// process can reach it, which is the property this flow's security rests on.
func (p *Provider) Mint(state, redirectURI, clientID string, id Identity) (string, error) {
	code, err := randomSecret()
	if err != nil {
		return "", err
	}
	token, err := randomSecret()
	if err != nil {
		return "", err
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked(now)
	p.codes[code] = &grant{
		state:       state,
		redirectURI: redirectURI,
		clientID:    clientID,
		identity:    id,
		accessToken: token,
		expires:     now.Add(codeTTL),
	}
	return code, nil
}

// CallbackQuery builds the query the hub's callback consumes for code, and
// refuses if state is not the one code was minted for. This is where the
// STATE binding is enforced: the hub never sends state to /token, which
// checks the other two bindings.
func (p *Provider) CallbackQuery(code, state string) (url.Values, error) {
	p.mu.Lock()
	g, ok := p.codes[code]
	p.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("remoteproxy: no such authorization code")
	}
	if subtle.ConstantTimeCompare([]byte(g.state), []byte(state)) != 1 {
		return nil, fmt.Errorf("remoteproxy: state does not match the code's binding")
	}
	return url.Values{"code": {code}, "state": {state}}, nil
}

// buildHandler registers the provider's routes. The route set is the whole
// security argument, so it is small and stated in one place.
func (p *Provider) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.handleDiscovery)
	mux.HandleFunc("/token", p.handleToken)
	mux.HandleFunc("/userinfo", p.handleUserinfo)
	mux.HandleFunc("/authorize", p.handleAuthorize)
	// Catch-all. Every path that is not one of the four above is a probe or a
	// misconfiguration, and both are worth seeing in the audit log.
	mux.HandleFunc("/", p.handleNotFound)
	return mux
}

// handleAuthorize is a permanent, deliberate 404. It must NEVER be
// implemented.
//
// scion's OIDC login validates nothing (see the file header): whoever obtains
// an authorization code gets a hub WEB SESSION for whatever email the
// provider asserts, an ADMIN session if server.hub.admin_emails were set. A
// working /authorize is an HTTP endpoint that mints codes, and this provider
// is reachable from every agent's network namespace at 169.254.1.2 (pasta
// --map-host-loopback; internal/backend/guest.EnsureRuntimes). Implementing
// it would let any jailed agent become a hub user.
//
// It is registered rather than absent so it does not read as an oversight to
// "finish". Discovery advertises DeadAuthorizationEndpoint instead; the proxy
// mints codes by calling Provider.Mint. A hit is a misconfiguration or an
// in-jail probe, so it is audited.
func (p *Provider) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	p.record(r, DecisionDenyAuthorize, http.StatusNotFound, "the local OIDC provider has no authorization endpoint by design")
	http.NotFound(w, r)
}

func (p *Provider) handleNotFound(w http.ResponseWriter, r *http.Request) {
	p.record(r, DecisionOIDCNotFound, http.StatusNotFound, "")
	http.NotFound(w, r)
}

// handleDiscovery serves exactly the fields scion reads plus the standard
// capability advertisements. jwks_uri is absent: no id_token is ever issued,
// and nothing serves one.
func (p *Provider) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	p.record(r, DecisionOIDCDiscovery, http.StatusOK, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                p.issuer,
		"authorization_endpoint":                DeadAuthorizationEndpoint,
		"token_endpoint":                        p.issuer + "/token",
		"userinfo_endpoint":                     p.issuer + "/userinfo",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"subject_types_supported":               []string{"public"},
		"scopes_supported":                      []string{"openid", "email", "profile"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

// handleToken redeems a code for an access token. The code is consumed on
// presentation, before any binding is checked, so a replay cannot be told
// from a first attempt by timing or by varying the other parameters.
func (p *Provider) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		p.record(r, DecisionOIDCTokenRefused, http.StatusMethodNotAllowed, "token endpoint takes POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "invalid_request"})
		return
	}
	if err := r.ParseForm(); err != nil {
		p.record(r, DecisionOIDCTokenRefused, http.StatusBadRequest, "malformed form body")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	g, why := p.redeem(r.PostForm.Get("code"), r.PostForm.Get("grant_type"),
		r.PostForm.Get("redirect_uri"), r.PostForm.Get("client_id"))
	if why != "" {
		// why is one of a fixed set of reasons; it never quotes the code, the
		// redirect_uri or anything else the caller sent.
		p.record(r, DecisionOIDCTokenRefused, http.StatusBadRequest, why)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	p.record(r, DecisionOIDCToken, http.StatusOK, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": g.accessToken,
		"token_type":   "Bearer",
		"expires_in":   int(tokenTTL.Seconds()),
		"scope":        "openid email profile",
	})
}

// redeem consumes code and checks every binding /token can see. It returns the
// grant, or a fixed reason string safe to audit.
func (p *Provider) redeem(code, grantType, redirectURI, clientID string) (*grant, string) {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	g, ok := p.codes[code]
	// Consume first: single-use is enforced by the delete, not by a flag that
	// a later branch might skip.
	delete(p.codes, code)
	switch {
	case !ok:
		return nil, "unknown or already-redeemed code"
	case now.After(g.expires):
		return nil, "code expired"
	case grantType != "authorization_code":
		return nil, "unsupported grant_type"
	case subtle.ConstantTimeCompare([]byte(g.redirectURI), []byte(redirectURI)) != 1:
		return nil, "redirect_uri does not match the code's binding"
	case subtle.ConstantTimeCompare([]byte(g.clientID), []byte(clientID)) != 1:
		return nil, "client_id does not match the code's binding"
	}
	g.expires = now.Add(tokenTTL)
	p.tokens[g.accessToken] = g
	p.sweepLocked(now)
	return g, ""
}

// handleUserinfo answers with the operator identity bound to the access
// token. email_verified is true because the proxy verified the identity at
// the front before the code was minted; scion refuses an unverified email.
func (p *Provider) handleUserinfo(w http.ResponseWriter, r *http.Request) {
	tok, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		p.record(r, DecisionOIDCUserinfoRefused, http.StatusUnauthorized, "no bearer token")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	now := p.now()
	p.mu.Lock()
	g, found := p.tokens[tok]
	if found && now.After(g.expires) {
		delete(p.tokens, tok)
		found = false
	}
	p.mu.Unlock()
	if !found {
		p.record(r, DecisionOIDCUserinfoRefused, http.StatusUnauthorized, "unknown or expired access token")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	p.record(r, DecisionOIDCUserinfo, http.StatusOK, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"sub":            g.identity.Subject,
		"email":          g.identity.Email,
		"email_verified": true,
		"name":           g.identity.Name,
	})
}

// sweepLocked drops expired codes and tokens. Called with p.mu held, on every
// mint and redemption. Both maps are keyed by unguessable secrets, so only a
// legitimate login adds to them.
func (p *Provider) sweepLocked(now time.Time) {
	for k, g := range p.codes {
		if now.After(g.expires) {
			delete(p.codes, k)
		}
	}
	for k, g := range p.tokens {
		if now.After(g.expires) {
			delete(p.tokens, k)
		}
	}
}

// record emits one audit line per provider request. TSLogin is left empty:
// the caller is the hub's back channel (or an in-jail prober), never a
// verified browser.
func (p *Provider) record(r *http.Request, decision Decision, status int, reason string) {
	if p.audit == nil {
		return
	}
	p.audit(AuditLine{
		Time:     time.Now().UTC(),
		Method:   truncateAudit(r.Method),
		Path:     truncateAudit(r.URL.Path),
		Decision: decision,
		Status:   status,
		Error:    reason,
	})
}

// bearerToken extracts the token from an Authorization header value. The
// scheme match is case-insensitive per RFC 7235; the token is returned as-is.
func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return header[len(prefix):], true
}

func randomSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("remoteproxy: generate secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
