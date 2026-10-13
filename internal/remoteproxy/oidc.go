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

// The local OIDC provider. scion's web layer authenticates a browser only by
// the scion_sess cookie, obtained without dev-auth through scion's generic
// OIDC login, so lever runs the smallest provider that path consumes.
//
// scion validates nothing on that path (no id_token, JWKS, PKCE, nonce or
// issuer check): it exchanges a code at /token and reads /userinfo. Security
// rests on one property: a code can only be created by an in-process call to
// Provider.Mint; no HTTP route mints one (see handleAuthorize). Discovery,
// /token and /userinfo are reachable from inside the jail (169.254.1.2), and
// without a code none of them yields a token, session or identity.

const (
	// LoginClientID is the client_id lever writes into the hub's oidc_login
	// block and the only one this provider mints for. A consistency check,
	// not a credential.
	LoginClientID = "lever-remote"

	// codeTTL bounds how long a minted code may sit unredeemed.
	codeTTL = 60 * time.Second

	// tokenTTL bounds the access token, which the hub uses once for
	// /userinfo. Not single-use: it grants only the identity lever already
	// chose, so a retry inside the hub's exchange should not fail.
	tokenTTL = 2 * time.Minute

	// secretBytes is the entropy of a code and an access token; the code is
	// the only secret in the flow.
	secretBytes = 32

	// DeadAuthorizationEndpoint is the advertised authorization_endpoint.
	// scion requires the field but nothing dials it. It deliberately does not
	// point at this provider's /authorize (see handleAuthorize). lever doctor
	// asserts the hub redirects here, proving it uses lever's provider.
	DeadAuthorizationEndpoint = "https://lever.invalid/authorize"
)

// Identity is the operator asserted at /userinfo: always the login the proxy
// already verified, never one this provider invents.
type Identity struct {
	Subject string // OIDC `sub`; stable per operator
	Email   string // the hub keys its user row on this
	Name    string // display name
}

type ProviderConfig struct {
	// Port is the host loopback port the provider listens on.
	Port int
	// IssuerPort is the guest-side loopback port the hub dials (the hub
	// refuses a non-loopback http issuer). It differs from Port because
	// OrbStack mirrors guest listeners onto the host (see
	// config.GuestLoginIssuerPort). Zero means Port.
	IssuerPort int
	// Audit receives one line per request; nil disables. Values never carry a
	// code, token or cookie.
	Audit func(line AuditLine)
}

// Provider is the host-side OIDC provider: three endpoints the hub consumes,
// plus Mint, which is not an endpoint.
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

// NewProvider builds the provider; Serve binds its listener.
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

// Port is the host loopback port, not the port the hub dials (see IssuerURL).
func (p *Provider) Port() int { return p.port }

// IssuerURL is the hub's oidc_login.issuer_url. It names the guest-side port
// because the hub dials it; the guest forwarder carries it to this host.
func (p *Provider) IssuerURL() string { return p.issuer }

// Handler serves discovery, /token, /userinfo, the /authorize 404, and a 404
// catch-all.
func (p *Provider) Handler() http.Handler { return p.handler }

// Mint creates an authorization code bound to one login attempt. Being a
// function call that only the proxy process can reach is the property the
// flow's security rests on.
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

// CallbackQuery builds the hub callback's query for code. It enforces the
// state binding, which /token cannot check because the hub never sends state.
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

// buildHandler's route set is the whole security argument; keep it small.
func (p *Provider) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.handleDiscovery)
	mux.HandleFunc("/token", p.handleToken)
	mux.HandleFunc("/userinfo", p.handleUserinfo)
	mux.HandleFunc("/authorize", p.handleAuthorize)
	// Anything else is a probe or misconfiguration, worth auditing.
	mux.HandleFunc("/", p.handleNotFound)
	return mux
}

// handleAuthorize is a permanent, deliberate 404. It must NEVER be
// implemented: scion's OIDC login validates nothing, so whoever gets a code
// gets a hub web session (admin, if admin_emails were set), and this provider
// is reachable from every agent netns at 169.254.1.2. An HTTP /authorize would
// let any jailed agent become a hub user. It is registered so it does not
// read as an oversight; a hit is audited.
func (p *Provider) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	p.record(r, DecisionDenyAuthorize, http.StatusNotFound, "the local OIDC provider has no authorization endpoint by design")
	http.NotFound(w, r)
}

func (p *Provider) handleNotFound(w http.ResponseWriter, r *http.Request) {
	p.record(r, DecisionOIDCNotFound, http.StatusNotFound, "")
	http.NotFound(w, r)
}

// handleDiscovery serves the fields scion reads. jwks_uri is absent: no
// id_token is ever issued.
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

// handleToken consumes the code before checking any binding, so a replay is
// indistinguishable from a first attempt by timing or parameters.
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
		// why is a fixed reason; it never quotes anything the caller sent.
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

// redeem returns the grant, or a fixed reason string safe to audit.
func (p *Provider) redeem(code, grantType, redirectURI, clientID string) (*grant, string) {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	g, ok := p.codes[code]
	// Single-use is enforced by this unconditional delete, not by a flag a
	// later branch might skip.
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

// handleUserinfo sets email_verified because the proxy verified the identity
// at the front before minting; scion refuses an unverified email.
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

// sweepLocked runs with p.mu held. Both maps are keyed by unguessable
// secrets, so only a legitimate login grows them.
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

// record leaves TSLogin empty: the caller is the hub's back channel or an
// in-jail prober, never a verified browser.
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

// bearerToken matches the scheme case-insensitively (RFC 7235).
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
