package remoteproxy

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// The local OIDC provider.
//
// scion's web layer authenticates a browser by ONE thing: the `scion_sess`
// cookie. It never reads Authorization (pkg/hub/web.go sessionAuthMiddleware),
// so the narrow PAT the proxy injects — enough for every /api/v1 call — cannot
// open the UI shell. The supported way to obtain that cookie without dev-auth
// is scion's generic OIDC login, so lever runs the smallest provider that
// login path actually consumes.
//
// What scion validates on that path: nothing. It requests no id_token, parses
// none, fetches no JWKS, uses neither PKCE nor a nonce, and never checks that
// the discovery document's issuer matches the configured one. It exchanges a
// code at /token and reads /userinfo. The security of this therefore does NOT
// come from OIDC. It comes from ONE property: an authorization code can only
// be created by an in-process call to Provider.Mint, inside the host-side
// proxy, at the same trust level as the remote PAT file sitting beside it.
// There is no HTTP route that mints one — see authorizeIsPermanently404.
//
// Everything the hub reaches (discovery, /token, /userinfo) is also reachable
// from inside the jail, because the guest-side forwarder that gives the hub a
// loopback issuer is reachable from every agent netns (lever maps guest
// loopback in at 169.254.1.2; see internal/backend/guest.EnsureRuntimes). That
// residual exposure is deliberate and bounded: without a code, none of those
// three endpoints yields a token, a session, or an identity.

const (
	// LoginClientID is the client_id lever writes into the hub's oidc_login
	// block (internal/backend/guest.EnsureHubLogin puts it there) and the only
	// one this provider will mint a code for. There is no client registry and
	// no client secret: with a single, local relying party, the client_id is a
	// consistency check between the hub's configuration and ours, not a
	// credential. Exported because the two ends of that contract are
	// configured by different packages and must not drift.
	LoginClientID = "lever-remote"

	// codeTTL bounds how long a minted code may sit unredeemed. The hub
	// redeems within one round trip of the callback, so this is generous
	// already; it exists so a code that never reaches the hub (a failed
	// callback, a killed request) cannot be redeemed later.
	codeTTL = 60 * time.Second

	// tokenTTL bounds the access token /token hands back. The hub uses it
	// once, immediately, for /userinfo. Deliberately NOT single-use: a retry
	// inside the hub's own exchange would otherwise fail closed for no
	// security gain, since the token grants nothing but the identity lever
	// already chose.
	tokenTTL = 2 * time.Minute

	// secretBytes is the entropy in a code and in an access token. 32 bytes
	// from crypto/rand — the code is the only secret in the whole flow.
	secretBytes = 32

	// DeadAuthorizationEndpoint is what discovery advertises for
	// authorization_endpoint. scion's discovery parse REQUIRES the field to
	// be present (pkg/hub/oidc_discovery.go returns an error without it, and
	// /auth/login/oidc then 500s) but nothing ever dials it: the proxy drives
	// the whole login server-side. It names a host that cannot resolve, so it
	// can never be mistaken for a live endpoint, and it deliberately does NOT
	// point at this provider's own /authorize — see authorizeIsPermanently404.
	// Exported so `lever doctor` can assert that the hub redirects HERE and
	// nowhere else, which is what proves the hub is configured against
	// lever's provider rather than someone's real IdP.
	DeadAuthorizationEndpoint = "https://lever.invalid/authorize"
)

// Identity is the operator the provider asserts at /userinfo. The proxy fills
// it from the tailnet login it already verified against remote.allowed_users
// (see Config.AllowedUsers), so the hub's user row reflects the identity the
// front end authenticated — not an identity this provider invented.
type Identity struct {
	Subject string // OIDC `sub`; stable per operator
	Email   string // the hub keys its user row on this
	Name    string // display name
}

// ProviderConfig configures NewProvider.
type ProviderConfig struct {
	// Port is the loopback port the provider listens on, ON THE HOST.
	Port int
	// IssuerPort is the port the HUB dials — the guest-side loopback port the
	// forwarder listens on, which the hub sees as local and therefore
	// accepts (a non-loopback http issuer makes it refuse to start). It is a
	// DIFFERENT number from Port: OrbStack mirrors a guest listener onto the
	// host at the same port, so one number for both halves left the provider
	// unable to bind its own (see backend.GuestLoginIssuerPort).
	//
	// Zero means "the same as Port", which is what a test wants when nothing
	// sits between the hub and the provider.
	IssuerPort int
	// Audit receives one line per request the provider answers; nil disables
	// (tests). Values never carry a code, token or cookie.
	Audit func(line AuditLine)
	// Now is the clock, injectable for expiry tests. nil ⇒ time.Now.
	Now func() time.Time
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

// grant is one minted authorization code and what it is bound to.
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
		now:    cfg.Now,
		codes:  map[string]*grant{},
		tokens: map[string]*grant{},
	}
	if p.now == nil {
		p.now = time.Now
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

// Mint creates an authorization code bound to one login attempt.
//
// This is the whole authorization step, and it is a function call: no route,
// no redirect, no HTTP. Only the proxy process can reach it, which is the
// single property the security of this flow rests on.
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
// refuses if state is not the one code was minted for.
//
// It is where the code's binding to STATE is enforced. The other two bindings
// are checked at /token, because the hub re-sends redirect_uri and client_id
// there — but it never sends state to /token, so a provider that only checked
// what /token carries would record a state binding it never enforced. The
// proxy is what chooses which state accompanies which code, so this is the
// point at which the pairing can still be wrong and still be caught.
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
// THE ESCALATION IT PREVENTS. scion's OIDC login path validates nothing (see
// the file header): whoever can obtain an authorization code can have the hub
// exchange it and receive a hub WEB SESSION for any email the provider
// asserts — an ADMIN session if server.hub.admin_emails were ever set. A
// working /authorize is exactly "an HTTP endpoint that mints codes". This
// provider is reachable from inside the jail, because the guest-side forwarder
// that gives the hub a loopback issuer is mapped into every agent's network
// namespace at 169.254.1.2 (lever configures pasta --map-host-loopback so
// agents can reach the VM-loopback hub; internal/backend/guest.EnsureRuntimes).
// So implementing this route would hand every jailed agent a path from "can
// reach the hub with a scoped token" to "IS a hub user" — created, in lever's
// own process, on request. That is strictly worse than any capability lever
// grants today.
//
// It is REGISTERED rather than simply absent on purpose: an absent route reads
// as an oversight and invites a future contributor to "finish" the provider.
// The discovery document advertises DeadAuthorizationEndpoint — not this path
// — because scion requires the field to exist, not because anything dials it.
// The proxy drives the whole login server-side and mints codes by calling
// Provider.Mint directly; nothing in a working setup ever arrives here.
//
// A hit is therefore either a misconfiguration or an in-jail probe, which is
// why it is audited.
func (p *Provider) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	p.record(r, "deny-authorize", http.StatusNotFound, "the local OIDC provider has no authorization endpoint by design")
	http.NotFound(w, r)
}

// authorizeIsPermanently404 is a named anchor for the decision above, so the
// grep that finds "/authorize" in this package also finds the reason it 404s.
// Referenced by TestAuthorizeIsPermanently404.
const authorizeIsPermanently404 = "/authorize must never be implemented: it would be an HTTP code-minting endpoint reachable from inside the jail"

func (p *Provider) handleNotFound(w http.ResponseWriter, r *http.Request) {
	p.record(r, "oidc-not-found", http.StatusNotFound, "")
	http.NotFound(w, r)
}

// discoveryDoc is the OIDC discovery document, holding exactly the fields
// scion reads plus the standard capability advertisements. jwks_uri is absent:
// the hub never fetches it (no id_token is ever issued or parsed), and
// advertising a URI nothing serves would be a lie.
func (p *Provider) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	p.record(r, "oidc-discovery", http.StatusOK, "")
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

// handleToken redeems a code for an access token.
//
// The code is consumed on presentation, before any binding is checked: a code
// that has been shown to this endpoint is spent whatever the outcome, so a
// replay cannot be distinguished from a first attempt by timing or by trying
// variations of the other parameters.
func (p *Provider) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		p.record(r, "oidc-token-refused", http.StatusMethodNotAllowed, "token endpoint takes POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "invalid_request"})
		return
	}
	if err := r.ParseForm(); err != nil {
		p.record(r, "oidc-token-refused", http.StatusBadRequest, "malformed form body")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	g, why := p.redeem(r.PostForm.Get("code"), r.PostForm.Get("grant_type"),
		r.PostForm.Get("redirect_uri"), r.PostForm.Get("client_id"))
	if why != "" {
		// why is one of a fixed set of reasons; it never quotes the code, the
		// redirect_uri or anything else the caller sent.
		p.record(r, "oidc-token-refused", http.StatusBadRequest, why)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	p.record(r, "oidc-token", http.StatusOK, "")
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
// token. email_verified is true because the identity did not come from a
// user-supplied claim at all: the proxy verified it at the tailnet edge before
// the code was minted. scion refuses a login whose email is unverified.
func (p *Provider) handleUserinfo(w http.ResponseWriter, r *http.Request) {
	tok, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		p.record(r, "oidc-userinfo-refused", http.StatusUnauthorized, "no bearer token")
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
		p.record(r, "oidc-userinfo-refused", http.StatusUnauthorized, "unknown or expired access token")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	p.record(r, "oidc-userinfo", http.StatusOK, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"sub":            g.identity.Subject,
		"email":          g.identity.Email,
		"email_verified": true,
		"name":           g.identity.Name,
	})
}

// sweepLocked drops expired codes and tokens. Called with p.mu held, on every
// mint and every redemption, so the maps track live logins rather than growing
// with every probe. Both maps are keyed by unguessable secrets, so nothing but
// a legitimate login ever adds to them in the first place.
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

// record emits one audit line per provider request. TSLogin is left empty: the
// caller is the hub's back channel (or an in-jail prober), never a browser
// whose tailnet identity the proxy verified.
func (p *Provider) record(r *http.Request, decision string, status int, reason string) {
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
	if len(header) <= len(prefix) || !equalFoldASCII(header[:len(prefix)], prefix) {
		return "", false
	}
	return header[len(prefix):], true
}

// equalFoldASCII compares two equal-length ASCII strings case-insensitively.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// randomSecret returns secretBytes of crypto/rand entropy, hex-encoded.
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
