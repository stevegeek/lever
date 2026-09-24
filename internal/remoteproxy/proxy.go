// Package remoteproxy is the host-side seam between the operator's tailnet
// and the jail hub's web UI. Auth is INJECTED here (the phone never holds a
// hub credential): every forwarded request, the SPA shell and /api/v1 alike,
// carries the verified operator's OWN hub web session, obtained host-side by
// the login driver (login.go), and nothing the client sent in Authorization
// or Cookie. So the hub authorizes each request as that operator's hub user,
// and the narrowing lives mostly in the hub: the lever-remote project role and
// the project-create access constraint `lever apply` grants that user (see
// internal/cli/host/remote_role.go). The proxy itself refuses the routes the
// hub does not narrow for that session (mintsCredential, refusedRoute). That injection is exactly why network
// provenance alone must never authenticate a browser-borne cross-site request: any website open on a
// tailnet device can make the browser send requests that arrive "from the
// tailnet". The origin rules below are therefore load-bearing security, not
// CORS hygiene. An unconfigured ServeHost fails closed: every request is
// refused, Origin-bearing or not, rather than let an accidental
// empty-string match decide. The hub also mints a fresh session cookie on
// every cookie-less request; that cookie is stripped from every response
// before it reaches the client, for the same reason — it would be an
// alternate, lever-unmanaged credential if it ever left the host.
//
// Precondition: this handler is safe to expose ONLY behind a loopback
// listener reached exclusively through `tailscale serve` (or equivalent) —
// the sole trustworthy source of a Tailscale-User-Login value. Every
// inbound Tailscale-* header is stripped before forwarding to the hub, so a
// client can never forge identity to the HUB; but the AllowedUsers check
// performed HERE still trusts whatever the listener's front-end set on the
// request. A directly reachable listener (LAN, a localhost port-forward, or
// a DNS rebind to the loopback address) lets any caller set
// Tailscale-User-Login itself and take the header-free allow path with the
// injected session. Enforcing the loopback bind is the caller's job (see the
// remote-serve CLI wiring). See the 2026-08-16 remote-agent-access design
// spec.
package remoteproxy

import (
	"cmp"
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/daemon"
)

// Config wires a Handler. All fields required unless noted.
type Config struct {
	// Target is the hub base URL AS SEEN FROM THE JAIL, e.g.
	// "http://127.0.0.1:8080" — the same address every other lever hub call
	// uses (scion.DefaultHubEndpoint). It is the URL, not the route: with
	// DialContext set, only the Host header and path come from here.
	Target *url.URL
	// DialContext, when non-nil, is how the proxy reaches Target. `lever
	// remote serve` sets JailDial, so each instance reaches its OWN guest hub
	// through its own jail instead of a host port that at most one instance
	// could own. Nil uses the default net dialer (tests).
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// ServeHost is the public origin's host (e.g. "mac.tail1234.ts.net").
	// Requests with an Origin header for any other host are rejected. An
	// empty ServeHost fails closed: EVERY request is refused, Origin-bearing
	// or not — an unconfigured host can never legitimately match, so treat
	// "unconfigured" as "deny all" rather than risk a silent empty-string
	// match (an Origin that parses to an empty host, e.g. "null", would
	// otherwise compare equal to an empty ServeHost).
	ServeHost string
	// ListenPort is the loopback port this proxy binds. It exists so
	// hostAllowed can admit a host-side probe (`lever doctor` dials
	// 127.0.0.1:<port>/healthz) without widening the Host allowlist to every
	// port. Zero admits the tailnet name only.
	ListenPort int
	// AllowedUsers, when non-empty, pins Tailscale-User-Login values.
	AllowedUsers []string
	// Session supplies the verified operator's hub web session, which is
	// the ONLY credential the proxy sends, on every request. Required: nil
	// refuses every request (503), since the proxy would otherwise forward
	// requests with no identity at all.
	//
	// One identity for the shell and the API, on purpose. scion's web layer
	// authenticates a browser by cookie alone (pkg/hub/web.go
	// sessionAuthMiddleware), and its API layer turns the same cookie into a
	// bearer token (sessionToBearerMiddleware) — but only when the request
	// carries NO Authorization header. The proxy used to inject the remote
	// PAT there, so the API ran as the PAT's owner while the SPA, the chat
	// and /events ran as the session user; scion's conversation model keys
	// DMs on the session user, so every chat read and send was a 403. Now
	// Rewrite strips Authorization and sends the session alone, so both
	// halves are the same hub user, and what that user may do is the
	// lever-remote role plus the project-create ceiling, both in the hub.
	Session SessionSource
	// Audit receives one line per decision; nil disables (tests).
	Audit func(line AuditLine)
	// LogPath is where the operator is told to look when the hub login
	// fails — the proxy's own log, named in that denial's response text.
	// Optional; "" uses DefaultLogPath.
	LogPath string
}

// DefaultLogPath is the proxy log location named in the hub-login-failed
// denial when Config.LogPath is unset: the remote proxy's stderr, relative to
// the instance root (see state.State.RemoteLog).
const DefaultLogPath = ".lever-state/remote.log"

// SessionSource hands out hub web sessions per verified operator, and takes
// them back when the hub stops honoring them. *LoginDriver implements it; the
// interface exists so the proxy can be exercised without a hub.
type SessionSource interface {
	// Cookie returns the scion_sess value for an operator login, logging in
	// on first use. Callers may race; implementations must share one attempt.
	Cookie(ctx context.Context, login string) (string, error)
	// Invalidate drops a session the hub rejected, so the next call logs in
	// again. The cookie value identifies WHICH session is being dropped, so a
	// late request cannot discard one a concurrent renewal just installed.
	Invalidate(login, cookie string)
}

// authAPIPrefix is the hub's own credential surface (pkg/hub/server.go
// registers the /api/v1/auth/* routes).
const authAPIPrefix = "/api/v1/auth/"

// mintsCredential reports whether a request under the hub's /api/v1/auth/
// surface is anything other than the few calls the web UI makes that hand out
// nothing. It is an ALLOWLIST, and everything else under the prefix is
// refused: that surface is where scion mints credentials, and a new minting
// route must fail closed rather than pass until someone notices. At this pin
// (pkg/hub/server.go, web.go) the refused routes include POST
// /api/v1/auth/tokens (a user access token — the session the proxy injects is
// a session credential, which requireSessionCredential lets mint one; scion's
// handleTokenByID routes "/api/v1/auth/tokens/" to the same create), the CLI
// device and authorize flows (/cli/*), /token, /refresh, /login,
// /integrations/google/exchange (answers a hub access token in its body for
// a Google token in the request, no session needed), /test-login, /validate
// and /invite/redeem (which would change the web user's role outside lever).
// A token in a response body would leave the host, breaking "the phone holds
// no hub credential" as surely as a Set-Cookie would.
//
// Allowed: GET me, admin-status, scopes, providers; POST logout; the SPA's
// token page listing (GET tokens), reading and deleting one (GET, DELETE
// tokens/<id>), and revoking one (POST tokens/<id>/revoke).
//
// p is the DECODED path (r.URL.Path), which is what scion's http.ServeMux
// matches on too: it unescapes a literal segment before matching, so
// "/api/v1/auth/%74okens" reaches the token handler and reads as it here.
// A path cleaning would change ("//api/v1/auth/tokens", "/api/v1/auth/./cli")
// is answered with a redirect, not dispatched, so it cannot reach a handler
// under a spelling this function misses.
func mintsCredential(method, p string) bool {
	if p == strings.TrimSuffix(authAPIPrefix, "/") {
		return true
	}
	if !strings.HasPrefix(p, authAPIPrefix) {
		return false
	}
	rest := strings.TrimPrefix(p, authAPIPrefix)
	switch rest {
	case "me", "admin-status", "scopes", "providers":
		return method != http.MethodGet && method != http.MethodHead
	case "logout":
		return method != http.MethodPost
	case "tokens":
		return method != http.MethodGet && method != http.MethodHead
	}
	if id, ok := strings.CutPrefix(rest, "tokens/"); ok && id != "" {
		if tok, sub, found := strings.Cut(id, "/"); found {
			return !(tok != "" && sub == "revoke" && method == http.MethodPost)
		}
		return method != http.MethodGet && method != http.MethodHead && method != http.MethodDelete
	}
	return true
}

// refusedRoute reports whether a request names a hub route the proxy never
// forwards, whatever the hub would decide. Each group below is one the hub's
// own authorization does not narrow for the session the proxy injects:
//
//   - /api/v1/system/* (pkg/hub/server.go, route class RouteWorkstation). These
//     run no RBAC at all: requireWorkstation only checks that the hub is not
//     hosted, and assertLoopback passes because the proxy reaches the hub from
//     127.0.0.1 inside the jail. They rewrite the image registry (PUT
//     registry), delete harness configs (POST init), rewrite the super-admin's
//     email (PUT identity), and list or create guest directories (fs/list,
//     fs/mkdir). Only GET status is forwarded: the SPA reads it on every load
//     (web/src/client/main.ts). An unknown route under the prefix is refused,
//     so a new one fails closed.
//   - Project creation: POST /api/v1/projects, /api/v1/projects/register and
//     POST /api/v1/projects/<id>/clone, and the legacy /api/v1/groves
//     aliases. Each one makes the caller the new project's owner. `lever
//     apply` caps project.create for the operator's hub user, but a user who
//     signs in for the first time holds hub-member (project.create included)
//     until the next apply binds them, so the proxy refuses these routes on
//     its own, regardless of hub state.
//   - /auth/callback/*: the hub's OAuth callback. Only the login driver needs
//     it, and the driver dials the hub directly (login.go), never through
//     this handler. A client that calls it with the injected session gets a
//     302 to /login?error=state_mismatch, which is noise at best.
//
// p is the DECODED path, as for mintsCredential.
func refusedRoute(method, p string) bool {
	if p == "/api/v1/system" || strings.HasPrefix(p, "/api/v1/system/") {
		return !(p == "/api/v1/system/status" && (method == http.MethodGet || method == http.MethodHead))
	}
	if p == callbackPath || strings.HasPrefix(p, callbackPath+"/") {
		return true
	}
	for _, base := range []string{"/api/v1/projects", "/api/v1/groves"} {
		if p == base {
			return method != http.MethodGet && method != http.MethodHead
		}
		rest, ok := strings.CutPrefix(p, base+"/")
		if !ok {
			continue
		}
		id, sub, _ := strings.Cut(rest, "/")
		if id == "register" {
			return true
		}
		if sub == "clone" || strings.HasPrefix(sub, "clone/") {
			return true
		}
	}
	return false
}

// callbackPath is the hub's OAuth callback route (pkg/hub/web.go
// handleOAuthCallback, /auth/callback/<provider>).
const callbackPath = "/auth/callback"

// loginPathPrefix is the hub's login route (it routes /auth/login/, see
// pkg/hub/web.go handleOAuthLogin). The SPA reaches it two ways: the shell's
// Sign-in link navigates to the bare path, and the login page's provider
// buttons to /auth/login/<provider>.
const loginPathPrefix = "/auth/login"

// isLoginPath reports whether p is the hub's login route, bare or with a
// provider. Not the callback route: refusedRoute refuses /auth/callback/
// outright, since the login driver reaches it without this handler.
func isLoginPath(p string) bool {
	return p == loginPathPrefix || strings.HasPrefix(p, loginPathPrefix+"/")
}

// loginReturnTarget picks where an intercepted sign-in navigation lands. The
// SPA's login page carries its own target as ?returnTo=<path>; that is
// preserved. Anything but an in-app absolute path falls back to "/": the
// value is caller-chosen text headed for a Location header, and an absolute
// or protocol-relative URL ("//evil.test", "/\evil.test" — browsers read a
// backslash as a slash there) would make the proxy an open redirect on its
// own origin.
func loginReturnTarget(v string) string {
	if !strings.HasPrefix(v, "/") ||
		strings.HasPrefix(v, "//") || strings.HasPrefix(v, "/\\") {
		return "/"
	}
	if u, err := url.Parse(v); err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return v
}

// Decision is an AuditLine outcome. Every value the package emits is a
// constant below; the gate (NewHandler), the login driver (login.go) and the
// OIDC provider (oidc.go) each use their own subset.
type Decision string

const (
	// Gate decisions (NewHandler).
	DecisionAllow         Decision = "allow"
	DecisionDenyHost      Decision = "deny-host"
	DecisionDenyOrigin    Decision = "deny-origin"
	DecisionDenyUser      Decision = "deny-user"
	DecisionDenyMint      Decision = "deny-credential-mint"
	DecisionDenyRoute     Decision = "deny-route"
	DecisionDenyNoSession Decision = "deny-no-session"
	DecisionLoginRedirect Decision = "login-redirect"
	// Login-driver decisions (login.go).
	DecisionOIDCSession       Decision = "oidc-session"
	DecisionOIDCSessionFailed Decision = "oidc-session-failed"
	// Provider decisions (oidc.go).
	DecisionOIDCDiscovery       Decision = "oidc-discovery"
	DecisionOIDCToken           Decision = "oidc-token"
	DecisionOIDCTokenRefused    Decision = "oidc-token-refused"
	DecisionOIDCUserinfo        Decision = "oidc-userinfo"
	DecisionOIDCUserinfoRefused Decision = "oidc-userinfo-refused"
	DecisionOIDCNotFound        Decision = "oidc-not-found"
	DecisionDenyAuthorize       Decision = "deny-authorize"
)

// AuditLine is emitted once per request, regardless of outcome. It never
// carries the session value.
type AuditLine struct {
	Time    time.Time `json:"time"`
	TSLogin string    `json:"ts_login,omitempty"`
	Method  string    `json:"method"`
	Path    string    `json:"path"`
	// Decision is the outcome: one of the Decision constants. The gate emits
	// DecisionAllow, DecisionDenyHost, DecisionDenyOrigin, DecisionDenyUser,
	// DecisionDenyMint, DecisionDenyRoute, DecisionDenyNoSession and, for an intercepted sign-in
	// navigation, DecisionLoginRedirect; the login driver DecisionOIDCSession
	// and DecisionOIDCSessionFailed; the provider the DecisionOIDC* values and
	// DecisionDenyAuthorize.
	Decision Decision `json:"decision"`
	Status   int      `json:"status,omitempty"`
	// Error records why an allowed request never got an answer from the hub
	// (set only on the 502 path). The transport's own diagnosis lands here
	// rather than in the client's response body: the operator needs to know
	// that, say, the jail has no nc, and the remote client must not.
	Error string `json:"error,omitempty"`
}

// maxAuditFieldLen caps how much caller-chosen text any single audit field
// carries. Path, Method and TSLogin are copied from the request on both sinks
// that write remote-audit.jsonl, and both sinks are reachable by someone whose
// text that is: the provider by anything inside the jail, and the proxy's gate
// by anything that reaches the listener — the gate audits every DENIAL too, so
// its line is written before any identity check has passed. (Error is lever's
// own wording, and Time, Decision and Status are not the caller's at all.) JSON encoding already makes an odd value
// unambiguous — this bound is about the operator's disk. One request may carry
// close to a megabyte of request line and headers (http.Server's default
// MaxHeaderBytes), so without a cap a few thousand of them append gigabytes to
// a file that lives in the same state directory as the broker's own data.
const maxAuditFieldLen = 200

// truncateAudit bounds one caller-chosen audit field. The ellipsis marks the
// value as cut, so a truncated identity or path is never read as a whole one.
// Callers must audit the truncated copy and DECIDE on the original: a
// truncated login would collide with every other login sharing its first
// maxAuditFieldLen bytes.
func truncateAudit(v string) string {
	if len(v) <= maxAuditFieldLen {
		return v
	}
	return v[:maxAuditFieldLen] + "…"
}

// responseHeaderTimeout bounds the wait for the hub's response headers.
// Generous on purpose: it is a last-resort guard against a wedged jail, not a
// latency budget. The hub answers a healthy request in milliseconds, and the
// slowest legitimate case — a cold hub still starting inside a machine that
// just came up — is far under this. Tests lower it on the built Transport
// (see setResponseHeaderTimeout in proxy_test.go).
const responseHeaderTimeout = 45 * time.Second

// secFetchSiteAllowed reports whether v is a Sec-Fetch-Site value a
// same-origin request or a user-initiated navigation can carry. Anything else
// is refused: an allowlist, not a denylist of "cross-site", so an
// unrecognized value fails closed instead of silently passing. "same-site" is
// refused too: another page on a sibling tailnet name (a different machine
// under the same ts.net registrable domain) is same-site, and the proxy
// answers only its own origin.
func secFetchSiteAllowed(v string) bool {
	switch strings.ToLower(v) {
	case "same-origin", "none":
		return true
	}
	return false
}

// ctxState carries the session and the in-flight AuditLine from the gate
// (which decides "allow") to the ReverseProxy hooks (which inject the session
// and, once the real upstream status is known, complete the audit call).
// Threaded through the request context because Rewrite/ModifyResponse/
// ErrorHandler only see *http.Request/*http.Response, not the gate's locals.
type ctxState struct {
	line *AuditLine
	// cookie is the hub session injected on THIS attempt.
	cookie string
	// retry is set by ModifyResponse when the hub rejected that session, and
	// read by the gate (to log in again) and by sessionRetryWriter (to
	// swallow the rejection instead of showing the operator a login page).
	// ModifyResponse runs BEFORE ReverseProxy writes anything to the
	// ResponseWriter, which is what makes one flag enough for both.
	retry bool
	// retryable records that this attempt may be repeated: the first attempt
	// of a request whose method carries no body. It stops a retry loop (the
	// second attempt sets it false) and keeps a POST from being replayed.
	retryable bool
	// stale is set by ModifyResponse when the hub rejected the session on a
	// first attempt that cannot be repeated (it has a body). The rejection
	// reaches the client as it is, and the gate drops the session so the
	// NEXT request logs in again instead of failing the same way.
	stale bool
	// retried marks the second attempt of a retried request, whose
	// rejection stands: a session the login just minted is not stale.
	retried bool
}

type ctxStateKey struct{}

// stateFrom returns the gate's ctxState for r, or nil when r never passed
// the gate (the ReverseProxy hooks are the only callers).
func stateFrom(r *http.Request) *ctxState {
	s, _ := r.Context().Value(ctxStateKey{}).(*ctxState)
	return s
}

// NewHandler returns the full middleware+proxy stack: the gate (origin,
// host, identity and path checks, session injection and one session retry)
// in front of the reverse proxy newReverseProxy builds.
func NewHandler(cfg Config) http.Handler {
	return &gate{cfg: cfg, rp: newReverseProxy(cfg)}
}

// newReverseProxy builds the upstream half: rewriteUpstream injects the
// session and strips every client-supplied identity;
// completeAudit strips the hub's cookie and completes the audit line;
// upstreamFailed does the same on the 502 path. With DialContext set it also
// owns the Transport (jailTransport).
func newReverseProxy(cfg Config) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{
		Rewrite:        rewriteUpstream(cfg.Target),
		ModifyResponse: completeAudit(cfg.Audit),
		ErrorHandler:   upstreamFailed(cfg.Audit),
	}
	if cfg.DialContext != nil {
		rp.Transport = jailTransport(cfg.DialContext)
	}
	return rp
}

// rewriteUpstream is the ReverseProxy Rewrite hook: point the request at
// target, strip every client-supplied identity, and attach the gate's
// session as the only one.
func rewriteUpstream(target *url.URL) func(*httputil.ProxyRequest) {
	return func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)
		// Strip any client-supplied identity — the injected session is the
		// only identity the hub ever sees. Authorization matters most:
		// scion's sessionToBearerMiddleware lets a request that carries
		// one straight through, so a phone-supplied bearer would choose
		// the identity the API runs as. The client's own Cookie header
		// goes too, or a client-supplied scion_sess would be honored as
		// an alternate credential. Tailscale-* headers are stripped as
		// well: the AllowedUsers gate trusts them (under the loopback-bind
		// precondition documented above), but the hub must never see a
		// client-supplied identity claim of its own.
		pr.Out.Header.Del("Authorization")
		pr.Out.Header.Del("Cookie")
		for k := range pr.Out.Header {
			if clientIdentityHeader(k) {
				pr.Out.Header.Del(k)
			}
		}
		if s := stateFrom(pr.In); s != nil && s.cookie != "" {
			pr.Out.Header.Set("Cookie", sessionCookieName+"="+s.cookie)
		}
		pr.SetXForwarded()
	}
}

// clientIdentityHeader reports whether a request header names an identity or
// a credential to the hub, other than Authorization and Cookie. None of these
// is ever the client's to send. Tailscale-* is trusted by the gate, never by
// the hub. The rest are the headers scion's auth middleware reads
// (pkg/hub/auth.go, brokerauth.go, federation_auth.go): X-Scion-* (agent
// token, broker HMAC headers, on-behalf-of, federation token, plugin name),
// the trusted-proxy X-Forwarded-User-* set, the IAP assertion and X-API-Key.
func clientIdentityHeader(k string) bool {
	k = strings.ToLower(k)
	switch {
	case strings.HasPrefix(k, "tailscale-"),
		strings.HasPrefix(k, "x-scion-"),
		strings.HasPrefix(k, "x-forwarded-user-"):
		return true
	}
	switch k {
	case "x-goog-iap-jwt-assertion", "x-api-key":
		return true
	}
	return false
}

// completeAudit is the ReverseProxy ModifyResponse hook: strip the hub's
// session cookie, flag a rejected session for the gate's one retry, and
// otherwise complete the audit line with the real upstream status.
func completeAudit(audit func(AuditLine)) func(*http.Response) error {
	return func(resp *http.Response) error {
		// The hub mints a fresh session cookie on every cookie-less
		// request. The client must never hold a hub credential, cookie
		// included. Header.Del operates on the canonicalized key, so
		// this removes every Set-Cookie value regardless of how many
		// the hub sent or what case it used.
		resp.Header.Del("Set-Cookie")
		if s := stateFrom(resp.Request); s != nil {
			if s.retryable && sessionRejected(resp) {
				// The hub does not know this session (it restarted, or the
				// session lapsed). Say so, and audit nothing: the gate
				// replaces the session and repeats the request, and that
				// attempt is the one that answers the operator. The login
				// itself is audited by the driver.
				s.retry = true
				return nil
			}
			if !s.retried && sessionRejected(resp) {
				// Same cause, but the request had a body and cannot be
				// replayed. Let the rejection through; the gate drops the
				// session so the next request heals it.
				s.stale = true
			}
			if s.line != nil && audit != nil {
				s.line.Status = resp.StatusCode
				audit(*s.line)
			}
		}
		return nil
	}
}

// upstreamFailed is the ReverseProxy ErrorHandler: the upstream round trip
// failed outright (hub down/unreachable), which bypasses ModifyResponse
// entirely. Complete the audit call here too, so "exactly one AuditLine per
// request" holds on this path as well.
func upstreamFailed(audit func(AuditLine)) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		if s := stateFrom(r); s != nil && s.line != nil {
			s.line.Status = http.StatusBadGateway
			if err != nil {
				s.line.Error = err.Error()
			}
			switch {
			case audit != nil:
				audit(*s.line)
			case err != nil:
				// No audit sink wired: the cause still must not vanish, or a
				// 502 says nothing about whether the jail or the hub is at
				// fault.
				daemon.Warnf("remote proxy upstream: %v", err)
			}
		}
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}
}

// jailTransport is the standard Transport with only its dial replaced. Not a
// hand-rolled RoundTripper: httputil.ReverseProxy leans on http.Transport's
// own 101/upgrade handling to carry the hub's WebSocket attach streams, and
// that is not part of the RoundTripper contract.
func jailTransport(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = dial
	// A jail dial has no host-side socket to proxy. Leaving Proxy set
	// would let an HTTP_PROXY in the operator's environment silently
	// redirect hub traffic — carrying the injected session — off the host.
	t.Proxy = nil
	// Nothing else bounds getting a response out of the hub. The jail
	// dial returns as soon as the child starts, so a wedged machine or a
	// hung jail transport binary would otherwise hold the request open
	// forever: no answer, no audit line, and a live child — the exact
	// diagnosable-502 this transport exists to produce, lost. This bounds
	// the headers only, so streamed bodies and upgraded connections run
	// as long as they like.
	t.ResponseHeaderTimeout = responseHeaderTimeout
	return t
}

// checkOrigin applies the browser-provenance rules (see the package doc):
// at most one Origin header, and it must name serveHost; at most one
// Sec-Fetch-Site header, and it must be a same-site value. It returns the
// denial and its response text, or "" when the request passes. A request
// carrying neither header passes — that is what hostAllowed is for.
func checkOrigin(r *http.Request, serveHost string) (Decision, string) {
	if origins := r.Header.Values("Origin"); len(origins) > 0 {
		if len(origins) > 1 {
			return DecisionDenyOrigin, "multiple Origin headers refused"
		}
		u, err := url.Parse(origins[0])
		if err != nil || u.Host == "" || !strings.EqualFold(u.Host, serveHost) {
			return DecisionDenyOrigin, "cross-origin request refused"
		}
	}
	if sfs := r.Header.Values("Sec-Fetch-Site"); len(sfs) > 0 {
		if len(sfs) > 1 {
			return DecisionDenyOrigin, "multiple Sec-Fetch-Site headers refused"
		}
		if !secFetchSiteAllowed(sfs[0]) {
			return DecisionDenyOrigin, "cross-site request refused"
		}
	}
	return "", ""
}

// gate is the request-side half of the handler: every check that decides
// whether a request reaches the hub, and the session plumbing around it.
type gate struct {
	cfg Config
	rp  *httputil.ReverseProxy
}

func (g *gate) audit(line AuditLine) {
	if g.cfg.Audit != nil {
		g.cfg.Audit(line)
	}
}

// deny answers the request with status/msg and audits the decision.
func (g *gate) deny(w http.ResponseWriter, line *AuditLine, status int, decision Decision, msg string) {
	line.Decision, line.Status = decision, status
	g.audit(*line)
	http.Error(w, msg, status)
}

// denyNoSession is the one denial three paths share: the hub login failed.
func (g *gate) denyNoSession(w http.ResponseWriter, line *AuditLine) {
	g.deny(w, line, http.StatusBadGateway, DecisionDenyNoSession,
		"hub login failed — see "+cmp.Or(g.cfg.LogPath, DefaultLogPath))
}

// ServeHTTP is authorize → (answer a sign-in navigation | attach the
// session) → forward. The audit line is opened here and completed by
// whichever of those answers the request.
func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := g.cfg
	// The identity the front end asserted, read once and used whole for
	// every DECISION below (the allowlist, the hub login, the session
	// cache key). The audit line gets bounded copies of it, of the path
	// and of the method: all three are caller-chosen text, this line is
	// written before any check has passed, and the provider's sink
	// already bounds what it writes to the same file. Deciding on the
	// truncated value instead would make every login sharing a
	// maxAuditFieldLen-byte prefix the same operator.
	login := r.Header.Get("Tailscale-User-Login")
	line := AuditLine{Time: time.Now().UTC(), TSLogin: truncateAudit(login), Method: truncateAudit(r.Method), Path: truncateAudit(r.URL.Path)}

	if !g.authorize(w, r, &line, login) {
		return
	}
	// The identity the hub is told about, which is NOT always the header:
	// the login is verified only when AllowedUsers pins it. See operatorFor.
	operator := g.operatorFor(login)
	line.Decision = DecisionAllow
	// Status is filled in by ModifyResponse/ErrorHandler once the
	// upstream round trip completes; the audit call happens there too,
	// not here, so the line carries the real status instead of the
	// zero value.
	state := &ctxState{line: &line}

	// A browser navigation to the hub's login route is answered HERE,
	// never forwarded: the hub would 302 it to the OIDC authorization
	// endpoint, which deliberately does not resolve
	// (DeadAuthorizationEndpoint) — the whole login is driven server-side
	// instead (see login.go). Run that driver, then send the browser back
	// into the app; if the session it hands out is a stale cached one,
	// the shell GETs that follow heal it through the retry in forward. The
	// driver cannot recurse into this branch: its own step 1 GETs
	// /auth/login/oidc with its own client, dialled straight at the hub.
	// /auth/callback/ never gets this far: authorize refuses it (see
	// refusedRoute), and the driver's own callback leg dials the hub
	// directly too.
	if isLoginPath(r.URL.Path) && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		g.serveLogin(w, r, &line, operator)
		return
	}

	// Every request rides the operator's own hub session (see
	// Config.Session). Obtaining it is lazy — the first request of the
	// proxy's life performs the login, and an instance nobody opens a
	// browser at never logs in at all.
	cookie, err := cfg.Session.Cookie(r.Context(), operator)
	if err != nil {
		g.denyNoSession(w, &line)
		return
	}
	state.cookie = cookie
	// Only a bodiless method may be repeated: the retry in forward re-runs
	// the request, and a body has already been consumed by then.
	state.retryable = r.Method == http.MethodGet || r.Method == http.MethodHead
	g.forward(w, r, state, operator)
}

// operatorFor is the identity the proxy asserts to the hub on the operator's
// behalf: the session cache key, and the login the OIDC provider mints a code
// for (identityFor). It is the Tailscale login ONLY when AllowedUsers pins it,
// because that check is the only thing that ever verifies the header: with
// the list empty the gate does not require the header at all, so its value
// is a claim nobody checked and must not become a hub user row. The hub then
// records the unnamed placeholder operator instead — which is what the
// remote-access guide documents, and one more reason to set allowed_users.
//
// Called AFTER authorize, so a non-empty AllowedUsers means login is in it.
func (g *gate) operatorFor(login string) string {
	if len(g.cfg.AllowedUsers) == 0 {
		return ""
	}
	return login
}

// authorize runs every check that decides whether the request may reach the
// hub at all — ServeHost configured, Host, Origin/Sec-Fetch-Site, the
// identity allowlist, a login driver wired, not a credential route — denying
// (and auditing) on the first failure. It reports whether the request passed.
func (g *gate) authorize(w http.ResponseWriter, r *http.Request, line *AuditLine, login string) bool {
	cfg := g.cfg
	// Fail closed on an unconfigured ServeHost: it can never
	// legitimately match a request's Origin, so refuse everything
	// rather than let an accidental empty-string comparison decide.
	// Applies regardless of whether this particular request carries an
	// Origin header at all.
	if cfg.ServeHost == "" {
		g.deny(w, line, http.StatusForbidden, DecisionDenyOrigin, "remote host not configured")
		return false
	}

	// Host first: it is the only gate a header-free request cannot walk
	// through. See hostAllowed.
	if !hostAllowed(r.Host, cfg.ServeHost, cfg.ListenPort) {
		g.deny(w, line, http.StatusForbidden, DecisionDenyHost, "unexpected Host")
		return false
	}

	if decision, msg := checkOrigin(r, cfg.ServeHost); decision != "" {
		g.deny(w, line, http.StatusForbidden, decision, msg)
		return false
	}
	if len(cfg.AllowedUsers) > 0 {
		// Duplicates are refused for the same reason Origin and
		// Sec-Fetch-Site are: Header.Get returns only the FIRST value, so a
		// second one is a header the gate silently ignores while something
		// downstream might not.
		if logins := r.Header.Values("Tailscale-User-Login"); len(logins) > 1 {
			g.deny(w, line, http.StatusForbidden, DecisionDenyUser, "multiple Tailscale-User-Login headers refused")
			return false
		}
		if !slices.Contains(cfg.AllowedUsers, login) {
			g.deny(w, line, http.StatusForbidden, DecisionDenyUser, "tailscale identity not allowed")
			return false
		}
	}
	if cfg.Session == nil {
		// No login driver: there is no identity to send, and forwarding
		// with none would only earn 401s from the hub.
		g.deny(w, line, http.StatusServiceUnavailable, DecisionDenyNoSession, "remote login not configured")
		return false
	}
	if mintsCredential(r.Method, r.URL.Path) {
		g.deny(w, line, http.StatusForbidden, DecisionDenyMint, "the remote proxy does not hand out hub credentials")
		return false
	}
	if refusedRoute(r.Method, r.URL.Path) {
		g.deny(w, line, http.StatusForbidden, DecisionDenyRoute, "the remote proxy does not forward this route")
		return false
	}
	return true
}

// serveLogin answers an intercepted sign-in navigation: drive the hub login
// for this operator, then send the browser back into the app.
func (g *gate) serveLogin(w http.ResponseWriter, r *http.Request, line *AuditLine, operator string) {
	if _, err := g.cfg.Session.Cookie(r.Context(), operator); err != nil {
		g.denyNoSession(w, line)
		return
	}
	line.Decision, line.Status = DecisionLoginRedirect, http.StatusFound
	g.audit(*line)
	http.Redirect(w, r, loginReturnTarget(r.URL.Query().Get("returnTo")), http.StatusFound)
}

// forward hands the authorized request to the reverse proxy. A retryable
// shell request is held back just long enough to learn whether the hub
// accepted the session; if it did not, the session is replaced and the
// request repeated once.
func (g *gate) forward(w http.ResponseWriter, r *http.Request, state *ctxState, operator string) {
	if !state.retryable {
		r = r.WithContext(context.WithValue(r.Context(), ctxStateKey{}, state))
		g.rp.ServeHTTP(w, r)
		if state.stale {
			g.cfg.Session.Invalidate(operator, state.cookie)
		}
		return
	}

	// Retryable shell request: hold the response back just long enough to
	// learn whether the hub accepted the session. If it did (the normal
	// case), everything streams through untouched.
	first := r.WithContext(context.WithValue(r.Context(), ctxStateKey{}, state))
	g.rp.ServeHTTP(&sessionRetryWriter{ResponseWriter: w, state: state}, first)
	if !state.retry {
		return
	}
	// The hub rejected the session. Replace it and answer the request
	// properly, rather than letting the operator's browser land on a
	// login page it cannot complete (the login is server-side; the SPA's
	// login button leads to an authorization endpoint that does not
	// resolve, by design — see Provider.handleAuthorize).
	g.cfg.Session.Invalidate(operator, state.cookie)
	cookie, err := g.cfg.Session.Cookie(r.Context(), operator)
	if err != nil {
		g.denyNoSession(w, state.line)
		return
	}
	// retryable is deliberately not set: one retry, then the hub's answer
	// stands whatever it is.
	again := &ctxState{line: state.line, cookie: cookie, retried: true}
	g.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxStateKey{}, again)))
}

// sessionRejected reports whether the hub's answer means "I do not know this
// session". scion says it two ways, depending on what it took the request for:
// a 401 for anything it reads as programmatic, and a 302 to its login page for
// anything it reads as a browser navigation (pkg/hub/web.go
// sessionAuthMiddleware). Nothing else is treated as a session problem — in
// particular a 403 is the hub refusing an ACTION, which a new session would
// not change. A login redirect that carries ?error= is not one either: scion
// sends those only from its OAuth callback (state_mismatch, no_code, …), and
// treating one as "session unknown" would let any client force a fresh login
// per request.
func sessionRejected(resp *http.Response) bool {
	if resp.StatusCode == http.StatusUnauthorized {
		return true
	}
	if resp.StatusCode != http.StatusFound {
		return false
	}
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || u.Scheme != "" || u.Host != "" {
		return false
	}
	if u.Path != "/login" && !isLoginPath(u.Path) {
		return false
	}
	return !u.Query().Has("error")
}

// sessionRetryWriter withholds a response the gate is about to replace.
//
// ReverseProxy calls ModifyResponse before it writes anything to the
// ResponseWriter, so by the time WriteHeader lands here the decision to retry
// is already made. On a retry the status, headers and body are all dropped —
// the client sees only the second attempt — and on everything else this is a
// pass-through: headers were written straight into the real ResponseWriter's
// map all along, so nothing is copied and a streamed body still streams.
type sessionRetryWriter struct {
	http.ResponseWriter
	state   *ctxState
	swallow bool
}

func (w *sessionRetryWriter) WriteHeader(status int) {
	if w.state.retry {
		// Clear the hub's rejection headers (Location, Content-Type, …) so
		// the retry writes into a clean response rather than inheriting them.
		clear(w.ResponseWriter.Header())
		w.swallow = true
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *sessionRetryWriter) Write(p []byte) (int, error) {
	if w.swallow {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

func (w *sessionRetryWriter) Flush() {
	if w.swallow {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer for everything
// this wrapper does not override — deadlines, and the Hijack that carries the
// hub's WebSocket upgrades. An upgrade cannot be swallowed anyway: it arrives
// as a 101, which is not a session rejection.
func (w *sessionRetryWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// hostAllowed reports whether a request asked for a name this proxy answers to.
//
// This is the DNS-rebinding defence, and it is the ONE check that does not
// depend on the client's goodwill. Origin and Sec-Fetch-Site are only present
// when a browser chooses to send them, so a request without them passes both
// gates by default. Binding to loopback is not a substitute either — loopback
// is exactly what a rebind targets: an attacker page on http://evil.test:8445
// whose DNS flips to 127.0.0.1 is, to the browser, SAME-ORIGIN with the proxy.
// It then sends no Origin, `Sec-Fetch-Site: same-origin`, and any header it
// likes (same-origin requests need no preflight), which before this check meant
// a forged Tailscale-User-Login and a reply carrying the injected credential's
// authority. Verified live 2026-08-22 against the running proxy: `Host:
// evil.example` + a forged identity returned 200 and real /auth/me data.
//
// A rebind cannot beat this because the browser sends the ATTACKER's name in
// Host — the victim navigated to their domain — while everything legitimate
// arrives as one of two names:
//
//   - serveHost, the tailnet name. `tailscale serve` forwards the client's Host
//     unchanged for a TCP backend, so real phone traffic carries it verbatim.
//   - loopback with the proxy's own port, for host-side probes: `lever doctor`
//     dials http://127.0.0.1:<port>/healthz.
//
// Nothing downstream depends on the inbound value: the outbound Host is
// rewritten by Rewrite (see newReverseProxy).
func hostAllowed(host, serveHost string, port int) bool {
	if host == "" {
		// HTTP/1.1 requires Host; Go rejects a request without one before
		// this. Treat the impossible case as hostile.
		return false
	}
	if strings.EqualFold(host, serveHost) {
		return true
	}
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		// No port in the header: only a bare serveHost match (above) counts.
		return false
	}
	if p != strconv.Itoa(port) {
		return false
	}
	switch strings.ToLower(strings.Trim(h, "[]")) {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}
