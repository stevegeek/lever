// Package remoteproxy is the host-side seam between the operator's front
// (the tailnet by default) and the jail hub's web UI. Auth is INJECTED here:
// every forwarded request carries the verified login's OWN hub web session,
// obtained host-side by the login driver (login.go), and nothing the client
// sent in Authorization or Cookie, so the browser never holds a hub
// credential. The hub authorizes each request as that login's hub user, and
// the narrowing lives mostly in the hub: the lever-remote project role and
// the project-create constraint `lever apply` grants that user (see
// internal/cli/host/remote_role.go). The proxy itself refuses the routes the
// hub does not narrow for that session (mintsCredential, refusedRoute).
//
// Because the credential is injected, network provenance must never
// authenticate a browser-borne cross-site request: any website open on a
// tailnet device can make the browser send requests that arrive "from the
// tailnet". The origin rules are load-bearing security, not CORS hygiene.
// The one cross-site request they admit is a top-level page load of an entry
// page (see entryNavigation), which changes nothing. An unconfigured
// ServeHost fails closed: every request is refused. The fresh session cookie
// the hub mints on every cookie-less request is stripped from every response,
// for the same reason.
//
// Precondition: this handler is safe ONLY behind a listener that nothing but
// the authenticating front can reach: `tailscale serve` by default, or
// another front that sets a verified login in the configured identity header
// (Config.IdentityHeader) and OVERWRITES any value the client sent. The
// identity headers are stripped before forwarding, so a client can never
// forge identity to the HUB, but the AllowedUsers check HERE trusts whatever
// the front set. A directly reachable listener (LAN, a localhost
// port-forward, a DNS rebind) lets any caller set the header itself and take
// the allow path with the injected session. Keeping the listener reachable
// only by the front is the caller's job: loopback by default, and a
// non-loopback bind only on an address the jail cannot reach, behind the
// operator's firewall (see config.validateRemoteBind).
package remoteproxy

import (
	"cmp"
	"context"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
)

// Config wires a Handler. All fields required unless noted.
type Config struct {
	// Target is the hub base URL AS SEEN FROM THE JAIL, e.g.
	// "http://127.0.0.1:8080" (scion.DefaultHubEndpoint). With DialContext
	// set, only the Host header and path come from here.
	Target *url.URL
	// DialContext, when non-nil, is how the proxy reaches Target. `lever
	// remote serve` sets JailDial, so each instance reaches its OWN guest hub
	// through its own jail. Nil uses the default net dialer (tests).
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// ServeHost is the public origin's host (e.g. "mac.tail1234.ts.net").
	// Requests with an Origin header other than https://<ServeHost> are
	// rejected (the host compared without case). An empty ServeHost fails
	// closed: EVERY request is refused, since an Origin that parses to an
	// empty host (e.g. "null") would otherwise compare equal.
	ServeHost string
	// ListenPort is the loopback port this proxy binds, so hostAllowed can
	// admit a host-side probe (`lever doctor` dials 127.0.0.1:<port>/healthz)
	// without admitting every port. Zero admits the tailnet name only.
	ListenPort int
	// AllowedUsers, when non-empty, pins IdentityHeader values.
	AllowedUsers []string
	// IdentityHeader is the header the authenticating front puts the verified
	// login in. "" = DefaultIdentityHeader (Tailscale-User-Login). It is read
	// for the AllowedUsers check and for the identity asserted to the hub,
	// and stripped from every forwarded request. The front must OVERWRITE it
	// (appending would let the client choose); a request carrying it twice,
	// or a comma-joined value, is refused (see authorize).
	IdentityHeader string
	// TrustForwardedHost makes the Host check read X-Forwarded-Host, when a
	// request carries one, instead of Host: for a front that rewrites Host.
	// Off by default, because any client that reaches the listener directly
	// can set that header (see hostToCheck).
	TrustForwardedHost bool
	// BindHost is the specific address the listener is bound to ("" for a
	// wildcard bind). hostAllowed then also admits "<BindHost>:<ListenPort>",
	// the Host a front sends when it dials that address and forwards no name.
	BindHost string
	// Session supplies the verified login's hub web session, the ONLY
	// credential the proxy sends, on every request. Nil refuses every request
	// (503).
	//
	// One identity for the shell and the API: scion authenticates a browser
	// by cookie alone, and its API turns the same cookie into a bearer only
	// when the request carries NO Authorization header. Rewrite strips
	// Authorization so both halves are the same hub user (scion keys DMs on
	// the session user), and what that user may do is the lever-remote role
	// plus the project-create ceiling, both in the hub.
	Session SessionSource
	// Audit receives one line per decision; nil disables (tests).
	Audit func(line AuditLine)
	// ChatLedger, when non-nil, receives one entry per web chat post the
	// hub accepted from a verified login (see recordChat and package
	// chatledger). Nil records nothing, and so nothing verifies. It is
	// never called when AllowedUsers is empty: then no login is verified.
	ChatLedger func(chatledger.Entry) error
	// Contacts maps each contact-tier login to the names of the agents it
	// may chat with (see contact.go). A login not in the map is an operator.
	// The ledger records the tier, so message_verify reports a contact's
	// message as a contact's.
	Contacts map[string][]string
	// ResolveAgents maps agent names to hub agent ids for the contact fence.
	// Required when Contacts is set: without it every contact request is
	// refused.
	ResolveAgents func(ctx context.Context) (map[string]string, error)
	// ContactSession reports why a contact may not post to the named agent
	// now, or nil when it may: the agent's session must have started fresh
	// with the lever skill that is on disk now (package sessionrec), since a
	// resumed session may still follow an older skill that trusts a marker a
	// contact can type. Nil refuses every contact post.
	ContactSession func(agent string) error
	// ChatAgent, when set, is the manager's agent name, and turns on lever's
	// chat page for every verified login, operator or contact (chatpage.go):
	// the proxy answers /lever/* itself and redirects "/" (and, for a
	// contact, every page) to the page. It needs AgentRecords for the agent
	// list, and a verified login (AllowedUsers). Empty leaves every one of
	// those paths to the hub (or, for a contact, the fence).
	ChatAgent string
	// Workers is every configured worker name in config order: the agents an
	// operator sees beside the manager on the chat page.
	Workers []string
	// ContactSee maps a contact login to the agents it may see on the chat
	// page (name, label, state) but not message.
	ContactSee map[string][]string
	// AgentRecords lists the instance's hub agent records by name, with phase
	// and activity already reduced to scion's known words. The chat page's
	// agent list needs it; nil answers every state "unknown".
	AgentRecords func(ctx context.Context) (map[string]AgentRecord, error)
	// Labels returns the labels by agent name (nil for none). Optional.
	Labels func() map[string]string
	// Wake asks the broker to resume a suspended (or, for the operator,
	// stopped) worker for a login that may message it (the operator socket's
	// /operator/wake); tier is the login's. It returns when the worker is
	// live or the broker refused; a refusal is a *WakeError with the
	// broker's status. Nil answers every wake 503.
	Wake func(ctx context.Context, login, tier, worker string) error
	// MatchAgentMessages (remote.agent_messages) reports which agent rows of
	// a contact's DM the broker's agent ledger recorded, by row id. A
	// contact is shown no other agent row, no event text, no DM preview,
	// and only shown rows count as unread (agentmsgs.go, events.go). Any
	// error hides every agent row. Nil = off: no contact answer is
	// rewritten.
	MatchAgentMessages func(ctx context.Context, contact, agent string, msgs []AgentMessage) (map[string]bool, error)
	// ContactUser reports the hub user id `lever apply` bound a contact login
	// to (remote-role.json), or false when it bound none: the contact never
	// signed in, or signed in after the last apply. The operator view
	// (opview.go) reads a contact's conversations only for a bound contact,
	// with that contact's own session; for an unbound one it asks the login
	// driver for nothing, since a login would create the contact's hub user.
	// Nil: no contact is bound.
	ContactUser func(login string) (string, bool)
	// PeekAgentMessages is MatchAgentMessages without binding: it answers
	// what the contact's own read would keep now, and the broker writes
	// nothing. The operator view uses it, so an operator's read never decides
	// which message a record shows. Nil while MatchAgentMessages is set: the
	// operator view treats every answer as unmatched (no agent row shown).
	// pending is the kept ids no contact read has bound yet (not shown to
	// the contact so far).
	PeekAgentMessages func(ctx context.Context, contact, agent string, msgs []AgentMessage) (keep, pending map[string]bool, err error)
	// Files (remote.files) turns on the chat page's file exchange: a login
	// uploads to an agent it may message and downloads what the agent
	// shared with it (files.go). Nil = off: every /lever/api/files/ path is
	// a 404. Needs ChatAgent.
	Files *FilesConfig
	// Voice (remote.voice) turns on dictation: the page records a clip and
	// /lever/api/voice/transcribe turns it into text for the message box
	// (voice.go); with Voice.Speech, /lever/api/voice/speak reads a part of
	// an agent message aloud with lever-tool-tts (speak.go). Nil = off: the
	// routes are a 404 and the roster carries no voice. Needs ChatAgent.
	Voice *VoiceConfig
	// Push, when non-nil, is the Web Push service (remote.push, push.go):
	// the page's subscription routes and worker, and the hub streams that
	// turn agent messages into pushes (the caller runs Push.Run). It needs
	// ChatAgent: without the chat page there is nothing to notify about.
	Push *Push
	// LogPath is the proxy log named in the hub-login-failed denial text.
	// "" uses DefaultLogPath.
	LogPath string
}

// DefaultIdentityHeader is Config.IdentityHeader when unset: the header
// `tailscale serve` puts the tailnet login in.
const DefaultIdentityHeader = "Tailscale-User-Login"

func (c Config) identityHeader() string {
	return http.CanonicalHeaderKey(cmp.Or(c.IdentityHeader, DefaultIdentityHeader))
}

// DefaultLogPath is the proxy log location named in the hub-login-failed
// denial when Config.LogPath is unset: the remote proxy's stderr, relative to
// the instance root (see state.State.RemoteLog).
const DefaultLogPath = ".lever-state/remote.log"

// SessionSource hands out hub web sessions per verified login, and takes
// them back when the hub stops honoring them. *LoginDriver implements it.
type SessionSource interface {
	// Cookie returns the scion_sess value for an operator login, logging in
	// on first use. Callers may race; implementations must share one attempt.
	Cookie(ctx context.Context, login string) (string, error)
	// Invalidate drops a session the hub rejected, so the next call logs in
	// again. The cookie value identifies WHICH session is being dropped, so a
	// late request cannot discard one a concurrent renewal just installed.
	Invalidate(login, cookie string)
}

// authAPIPrefix is the hub's own credential surface.
const authAPIPrefix = "/api/v1/auth/"

// mintsCredential reports whether a request under the hub's /api/v1/auth/
// surface is anything other than the few calls the web UI makes that hand
// out nothing. It is an ALLOWLIST: that surface is where scion mints
// credentials, and a new minting route must fail closed. The refused routes
// include POST /api/v1/auth/tokens (the injected session may mint a user
// access token; "/api/v1/auth/tokens/" routes to the same create), the CLI
// device and authorize flows (/cli/*), /token, /refresh, /login,
// /integrations/google/exchange (answers a hub access token for a Google
// token, no session needed), /test-login, /validate and /invite/redeem
// (which would change the web user's role outside lever). A token in a
// response body would leave the host as surely as a Set-Cookie would.
//
// Allowed: GET me, admin-status, scopes, providers; POST logout; the SPA's
// token page listing (GET tokens), reading and deleting one (GET, DELETE
// tokens/<id>), and revoking one (POST tokens/<id>/revoke).
//
// p is the DECODED path (r.URL.Path), which is what scion's http.ServeMux
// matches on too ("/api/v1/auth/%74okens" reaches the token handler). A path
// cleaning would change ("//api/v1/auth/tokens", "/api/v1/auth/./cli") is
// answered with a redirect, not dispatched.
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
// forwards, whatever the hub would decide. Each group is one the hub's own
// authorization does not narrow for the injected session:
//
//   - /api/v1/system/* (route class RouteWorkstation). These run no RBAC:
//     requireWorkstation only checks that the hub is not hosted, and
//     assertLoopback passes because the proxy reaches the hub from 127.0.0.1
//     inside the jail. They rewrite the image registry, delete harness
//     configs, rewrite the super-admin's email, and list or create guest
//     directories. Only GET status is forwarded (the SPA reads it on every
//     load). An unknown route under the prefix is refused, so a new one
//     fails closed.
//   - Project creation: POST /api/v1/projects, /api/v1/projects/register and
//     POST /api/v1/projects/<id>/clone, and the legacy /api/v1/groves
//     aliases. Each makes the caller the new project's owner. A user who
//     signs in for the first time holds hub-member (project.create included)
//     until the next `lever apply` binds them, so the proxy refuses these
//     on its own, regardless of hub state.
//   - /auth/callback/*: only the login driver needs it, and it dials the hub
//     directly (login.go), never through this handler.
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

// callbackPath is the hub's OAuth callback route (/auth/callback/<provider>).
const callbackPath = "/auth/callback"

// loginPathPrefix is the hub's login route. The shell's Sign-in link
// navigates to the bare path, and the login page's provider buttons to
// /auth/login/<provider>.
const loginPathPrefix = "/auth/login"

// isLoginPath reports whether p is the hub's login route, bare or with a
// provider. Not the callback route: refusedRoute refuses /auth/callback/
// outright, since the login driver reaches it without this handler.
func isLoginPath(p string) bool {
	return p == loginPathPrefix || strings.HasPrefix(p, loginPathPrefix+"/")
}

// loginReturnTarget picks where an intercepted sign-in navigation lands: the
// SPA's ?returnTo=<path>, or "/" for anything but an in-app absolute path.
// The value is caller-chosen text headed for a Location header; an absolute
// or protocol-relative URL ("//evil.test", "/\evil.test": browsers read a
// backslash as a slash there) would make the proxy an open redirect.
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
// constant below.
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
	Time time.Time `json:"time"`
	// TSLogin is the value of the configured identity header, as the front
	// sent it. The name and JSON key are kept for existing audit readers.
	TSLogin string `json:"ts_login,omitempty"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	// Decision is the outcome: one of the Decision constants.
	Decision Decision `json:"decision"`
	Status   int      `json:"status,omitempty"`
	// Reason is a fixed word for why the chat page's wake route refused or
	// how a late wake ended (wake.go), or, on an allowed line, that the
	// request was a cross-site or same-site page load the gate admitted
	// (entryNavigation); never caller or broker text.
	Reason string `json:"reason,omitempty"`
	// Contact and Agent name the conversation an operator-view read was for,
	// and Count how many rows (or contacts) it answered (opview.go). Never
	// message text.
	Contact string `json:"contact,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Count   *int   `json:"count,omitempty"`
	// AudioSeconds and LatencyMS describe a dictation clip (voice.go): its
	// length, and how long the transcription took. Never the audio, never
	// the text.
	AudioSeconds float64 `json:"audio_seconds,omitempty"`
	LatencyMS    int64   `json:"latency_ms,omitempty"`
	// MessageID and Chars describe a read-aloud request (speak.go): the
	// message the page says the text is from (with Agent), and the text's
	// length in characters. Never the text, never the audio.
	MessageID string `json:"message_id,omitempty"`
	Chars     int    `json:"chars,omitempty"`
	// Error records why an allowed request never got an answer from the hub
	// (the 502 path). The transport's diagnosis lands here, not in the
	// client's response body.
	Error string `json:"error,omitempty"`
}

// maxAuditFieldLen caps how much caller-chosen text one audit field carries.
// Path, Method and TSLogin are copied from the request, and both sinks that
// write remote-audit.jsonl are reachable by whoever chose that text: the
// provider from inside the jail, and the gate, which audits denials before
// any identity check. One request may carry close to a megabyte of headers,
// so without a cap a few thousand requests fill the operator's disk.
const maxAuditFieldLen = 200

// truncateAudit bounds one caller-chosen audit field; the ellipsis marks the
// value as cut. Callers must audit the truncated copy and DECIDE on the
// original: a truncated login would collide with every login sharing its
// prefix.
func truncateAudit(v string) string {
	if len(v) <= maxAuditFieldLen {
		return v
	}
	return v[:maxAuditFieldLen] + "…"
}

// responseHeaderTimeout bounds the wait for the hub's response headers: a
// last-resort guard against a wedged jail, not a latency budget. Tests lower
// it on the built Transport (setResponseHeaderTimeout).
const responseHeaderTimeout = 45 * time.Second

// secFetchSiteAllowed reports whether v is a Sec-Fetch-Site value a
// same-origin request or a user-initiated navigation can carry. It is an
// allowlist, so an unrecognized value fails closed. "same-site" is refused
// too: a sibling tailnet name under the same ts.net domain is same-site, and
// the proxy answers only its own origin.
func secFetchSiteAllowed(v string) bool {
	switch strings.ToLower(v) {
	case "same-origin", "none":
		return true
	}
	return false
}

// entryNavigation is the one exception to secFetchSiteAllowed: a top-level
// page load of an entry page, from a link in another app or site or from the
// redirect back from a front's login page. Those carry Sec-Fetch-Site
// cross-site (or same-site) across every redirect after. It admits exactly
// that:
//
//   - GET or HEAD, with exactly one Sec-Fetch-Mode, "navigate", and exactly
//     one Sec-Fetch-Dest, "document" (topLevelNavigation). Frames, fetches,
//     subresources and WebSockets say otherwise.
//   - No Origin header. A browser sends none on a GET navigation; an Origin
//     ("null" included) is not a plain page load.
//   - Only to an entry page (entryPage): "/" and, with the chat page on,
//     /lever/chat. Neither changes anything on GET. Each answer forbids
//     framing, and a cross-site opener cannot read a cross-origin page. The
//     page's own later requests are same-origin and meet the strict rule.
//
// The hub session is injected only on forward, as for every request, so the
// navigation gains nothing a typed URL would not. The audit line records it
// (navigationReason).
func entryNavigation(r *http.Request, site string, chatPage bool) bool {
	switch strings.ToLower(site) {
	case "cross-site", "same-site":
	default:
		return false
	}
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		len(r.Header.Values("Origin")) == 0 &&
		topLevelNavigation(r) && entryPage(r.URL.Path, chatPage)
}

// topLevelNavigation reports whether r says it is a top-level page load:
// exactly one Sec-Fetch-Mode, "navigate", and exactly one Sec-Fetch-Dest,
// "document".
func topLevelNavigation(r *http.Request) bool {
	mode, dest := r.Header.Values("Sec-Fetch-Mode"), r.Header.Values("Sec-Fetch-Dest")
	return len(mode) == 1 && strings.EqualFold(mode[0], "navigate") &&
		len(dest) == 1 && strings.EqualFold(dest[0], "document")
}

// entryPage reports whether p is a page a link from outside may open: "/",
// and the chat page when it is on. p is the decoded path.
func entryPage(p string, chatPage bool) bool {
	return p == "/" || (chatPage && p == chatPagePath)
}

// Audit reasons for a request entryNavigation admitted, by its
// Sec-Fetch-Site.
const (
	ReasonCrossSiteNavigation = "cross-site-navigation"
	ReasonSameSiteNavigation  = "same-site-navigation"
)

// navigationReason is the audit reason for a request checkOrigin admitted:
// a navigation word when its Sec-Fetch-Site is one only entryNavigation
// admits, "" otherwise.
func navigationReason(r *http.Request) string {
	switch strings.ToLower(r.Header.Get("Sec-Fetch-Site")) {
	case "cross-site":
		return ReasonCrossSiteNavigation
	case "same-site":
		return ReasonSameSiteNavigation
	}
	return ""
}

// denyFraming forbids framing a hub answer to an entry page, without leaning
// on the hub's X-Frame-Options. A second CSP header only narrows the hub's.
func denyFraming(resp *http.Response) {
	if resp.Request == nil || !entryPage(resp.Request.URL.Path, true) {
		return
	}
	resp.Header.Add("Content-Security-Policy", "frame-ancestors 'none'")
	resp.Header.Set("X-Frame-Options", "DENY")
}

// ctxState carries the session and the in-flight AuditLine from the gate to
// the ReverseProxy hooks, which see only *http.Request/*http.Response.
type ctxState struct {
	line   *AuditLine
	cookie string
	// retry is set by ModifyResponse when the hub rejected that session, and
	// read by the gate (to log in again) and by sessionRetryWriter (to
	// swallow the rejection). ModifyResponse runs BEFORE ReverseProxy writes
	// anything, which is what makes one flag enough for both.
	retry bool
	// retryable: the first attempt of a bodiless request. The second attempt
	// sets it false, so there is one retry and a POST is never replayed.
	retryable bool
	// stale is set when the hub rejected the session on a first attempt that
	// cannot be repeated (it has a body): the rejection stands, and the gate
	// drops the session so the NEXT request logs in again.
	stale bool
	// retried marks the second attempt of a retried request, whose
	// rejection stands: a session the login just minted is not stale.
	retried bool
	// login is the verified login this request runs as ("" when
	// AllowedUsers is empty and nothing verified it). recordChat reads it.
	login string
	// keepDM, when set, filters the hub's /api/v1/chat/dms answer to the
	// conversations it allows (the contact fence).
	keepDM func(key string) bool
	// rewrite, when set, rewrites the hub's answer for a contact
	// (remote.agent_messages): after keepDM, on every attempt.
	rewrite func(*http.Response)
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
	g := &gate{cfg: cfg, rp: newReverseProxy(cfg)}
	if len(cfg.Contacts) > 0 && cfg.ResolveAgents != nil {
		g.contacts = &contactFence{resolve: cfg.ResolveAgents, whoAmI: hubWhoAmI(cfg), session: cfg.ContactSession}
	}
	if cfg.ChatAgent != "" {
		g.chat = newChatPage(cfg)
		if cfg.Files != nil {
			g.files = newFilesState(*cfg.Files)
		}
		if cfg.Voice != nil {
			g.voice = newVoiceState(*cfg.Voice)
			if cfg.Voice.Speech != nil {
				g.speak = newSpeakState(*cfg.Voice.Speech)
			}
		}
		if cfg.Push != nil {
			g.push = cfg.Push
			cfg.Push.attach(g)
		}
	}
	return g
}

// newReverseProxy builds the upstream half: rewriteUpstream, completeAudit,
// and upstreamFailed for the 502 path. With DialContext set it also owns the
// Transport (jailTransport).
func newReverseProxy(cfg Config) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{
		Rewrite:        rewriteUpstream(cfg.Target, cfg.identityHeader()),
		ModifyResponse: completeAudit(cfg.Audit, cfg.ChatLedger, cfg.Contacts),
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
func rewriteUpstream(target *url.URL, identityHeader string) func(*httputil.ProxyRequest) {
	return func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)
		// The injected session is the only identity the hub ever sees.
		// Authorization matters most: scion lets a request carrying one
		// straight through, so a client-supplied bearer would choose the
		// identity. The client's Cookie goes too, or a client-supplied
		// scion_sess would be honored. The identity header and Tailscale-*
		// are trusted by the gate but must never reach the hub.
		pr.Out.Header.Del("Authorization")
		pr.Out.Header.Del("Cookie")
		pr.Out.Header.Del(identityHeader)
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
// the hub. The rest are the headers scion's auth middleware reads: X-Scion-*
// (agent token, broker HMAC, on-behalf-of, federation token, plugin name),
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
func completeAudit(audit func(AuditLine), ledger func(chatledger.Entry) error, contacts map[string][]string) func(*http.Response) error {
	return func(resp *http.Response) error {
		// The client must never hold a hub credential, including the
		// fresh cookie the hub mints on every cookie-less request.
		// Header.Del canonicalizes the key, so every Set-Cookie goes.
		resp.Header.Del("Set-Cookie")
		sandboxAPIDocument(resp)
		denyFraming(resp)
		if s := stateFrom(resp.Request); s != nil && (s.keepDM != nil || s.rewrite != nil) {
			failOtherSuccess(resp)
		}
		if s := stateFrom(resp.Request); s != nil && s.keepDM != nil {
			filterDMList(resp, s.keepDM)
		}
		if s := stateFrom(resp.Request); s != nil && s.rewrite != nil && !(s.retryable && sessionRejected(resp)) {
			s.rewrite(resp)
		}
		if s := stateFrom(resp.Request); s != nil {
			if s.retryable && sessionRejected(resp) {
				// The hub does not know this session. Audit nothing: the gate
				// replaces the session and repeats the request, and that
				// attempt answers the operator. The driver audits the login.
				s.retry = true
				return nil
			}
			if !s.retried && sessionRejected(resp) {
				// The request had a body and cannot be replayed. Let the
				// rejection through; the gate drops the session.
				s.stale = true
			}
			recordChat(resp, s.login, tierOf(contacts, s.login), ledger, func(err error) { slog.Warn("chat post not recorded", "err", err) })
			if s.line != nil && audit != nil {
				s.line.Status = resp.StatusCode
				audit(*s.line)
			}
		}
		return nil
	}
}

// failOtherSuccess makes a 2xx answer other than 200 a bodiless 502. The
// contact filters (keepDM, rewrite) read only a 200 answer and pass any
// other unread, so another success must not keep the hub's body.
func failOtherSuccess(resp *http.Response) {
	if resp.StatusCode == http.StatusOK || resp.StatusCode < 200 || resp.StatusCode > 299 {
		return
	}
	_ = resp.Body.Close()
	resp.StatusCode, resp.Status = http.StatusBadGateway, "502 Bad Gateway"
	resp.Header.Del("Content-Type")
	setBody(resp, nil)
}

// sandboxAPIDocument puts every /api/ response in a CSP sandbox.
//
// The hub serves agent-written files under /api/ inline, with a type taken
// from the file name, and its CSP allows inline script. Opened as a page
// through this proxy, such a file would run on the SPA's origin with the
// injected session: an agent could send the operator a link and act as them
// (attach to any agent, or post chat that verifies as the operator's). A
// sandboxed document has an opaque origin, so its requests carry
// "Origin: null", which checkOrigin refuses, and it may not run script or
// submit forms. The SPA reads /api/ with fetch, where a response's CSP has
// no effect. nosniff keeps a browser from reading JSON or text as HTML.
func sandboxAPIDocument(resp *http.Response) {
	if resp.Request == nil || !strings.HasPrefix(resp.Request.URL.Path, "/api/") {
		return
	}
	resp.Header.Add("Content-Security-Policy", "sandbox")
	resp.Header.Set("X-Content-Type-Options", "nosniff")
}

// upstreamFailed is the ReverseProxy ErrorHandler: the round trip failed
// outright, bypassing ModifyResponse, so the audit line is completed here
// too (exactly one AuditLine per request).
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
				// No audit sink wired: the cause still must not vanish.
				slog.Warn("upstream request failed", "err", err)
			}
		}
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}
}

// jailTransport is the standard Transport with only its dial replaced. Not a
// hand-rolled RoundTripper: httputil.ReverseProxy relies on http.Transport's
// own 101/upgrade handling to carry the hub's WebSocket attach streams.
func jailTransport(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = dial
	// An HTTP_PROXY in the operator's environment must never redirect hub
	// traffic, carrying the injected session, off the host.
	t.Proxy = nil
	// The jail dial returns as soon as the child starts, so without this a
	// wedged machine would hold the request open forever, with no answer and
	// no audit line. It bounds the headers only: streamed bodies and upgraded
	// connections run on.
	t.ResponseHeaderTimeout = responseHeaderTimeout
	return t
}

// checkOrigin applies the browser-provenance rules (see the package doc):
// at most one Origin header, and it must be https://<serveHost>; at most one
// Sec-Fetch-Site header, and it must be same-origin or none, except on a
// top-level navigation to an entry page (entryNavigation; chatPage says
// whether /lever/chat is one). It returns the denial and its response text,
// or "" when the request passes. A request carrying neither header passes —
// that is what hostAllowed is for.
func checkOrigin(r *http.Request, serveHost string, chatPage bool) (Decision, string) {
	if origins := r.Header.Values("Origin"); len(origins) > 0 {
		if len(origins) > 1 {
			return DecisionDenyOrigin, "multiple Origin headers refused"
		}
		if !isServeOrigin(origins[0], serveHost) {
			return DecisionDenyOrigin, "cross-origin request refused"
		}
	}
	if sfs := r.Header.Values("Sec-Fetch-Site"); len(sfs) > 0 {
		if len(sfs) > 1 {
			return DecisionDenyOrigin, "multiple Sec-Fetch-Site headers refused"
		}
		if !secFetchSiteAllowed(sfs[0]) && !entryNavigation(r, sfs[0], chatPage) {
			return DecisionDenyOrigin, "cross-site request refused"
		}
	}
	return "", ""
}

// gate is the request-side half of the handler: every check that decides
// whether a request reaches the hub, and the session plumbing around it.
type gate struct {
	cfg      Config
	rp       *httputil.ReverseProxy
	contacts *contactFence // nil unless Config.Contacts and ResolveAgents are set
	chat     *chatPage     // nil unless Config.ChatAgent is set
	push     *Push         // nil unless Config.Push and ChatAgent are set
	files    *filesState   // nil unless Config.Files and ChatAgent are set
	voice    *voiceState   // nil unless Config.Voice and ChatAgent are set
	speak    *speakState   // nil unless Config.Voice.Speech is set too
	wakes    wakeLimiter   // the chat page's wake route, one per agent a minute
	// wakeWaitFor overrides wakeAnswerWait (tests).
	wakeWaitFor time.Duration
}

func (g *gate) audit(line AuditLine) {
	if g.cfg.Audit != nil {
		g.cfg.Audit(line)
	}
}

func (g *gate) deny(w http.ResponseWriter, line *AuditLine, status int, decision Decision, msg string) {
	line.Decision, line.Status = decision, status
	g.audit(*line)
	http.Error(w, msg, status)
}

// denyNavigation is deny for a top-level page load: the same status and
// decision, answered as a small page with a link back to the landing path
// instead of text/plain, which a phone may offer as a download. The page has
// no script, and msg is lever's own text, escaped all the same.
func (g *gate) denyNavigation(w http.ResponseWriter, r *http.Request, line *AuditLine, status int, decision Decision, msg string) {
	line.Decision, line.Status = decision, status
	g.audit(*line)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, refusalPage(msg))
	}
}

// refusalPage is denyNavigation's body. Its link is a same-origin navigation
// from this page, which the gate admits.
func refusalPage(msg string) string {
	return `<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width, initial-scale=1"><title>Refused</title></head>` +
		`<body><p>` + html.EscapeString(msg) + `</p><p><a href="/">Open the chat</a></p></body></html>` + "\n"
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
	// The front's identity is used whole for every DECISION below (the
	// allowlist, the hub login, the session cache key). The audit line gets
	// bounded copies of it, the path and the method: all caller-chosen,
	// written before any check has passed. Deciding on a truncated value
	// would merge every login sharing a maxAuditFieldLen-byte prefix.
	login := r.Header.Get(cfg.identityHeader())
	line := AuditLine{Time: time.Now().UTC(), TSLogin: truncateAudit(login), Method: truncateAudit(r.Method), Path: truncateAudit(r.URL.Path)}
	if msg := unsafePath(r); msg != "" {
		g.deny(w, &line, http.StatusBadRequest, DecisionDenyPath, msg)
		return
	}

	if !g.authorize(w, r, &line, login) {
		return
	}
	// The identity the hub is told about, which is NOT always the header:
	// the login is verified only when AllowedUsers pins it. See operatorFor.
	operator := g.operatorFor(login)
	line.Decision = DecisionAllow
	// Status is filled in, and the audit call made, by
	// ModifyResponse/ErrorHandler once the upstream round trip completes.
	state := &ctxState{line: &line, login: operator}

	// A browser navigation to the hub's login route is answered HERE,
	// never forwarded: the hub would 302 it to the OIDC authorization
	// endpoint, which deliberately does not resolve. Run the driver, then
	// send the browser back into the app; a stale cached session heals
	// through the retry in forward. The driver cannot recurse into this
	// branch: it dials the hub directly. /auth/callback/ never gets this
	// far (refusedRoute).
	if isLoginPath(r.URL.Path) && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		g.serveLogin(w, r, &line, operator)
		return
	}

	// Every request rides the login's own hub session (see Config.Session),
	// obtained lazily on the first request.
	cookie, err := cfg.Session.Cookie(r.Context(), operator)
	if err != nil {
		g.denyNoSession(w, &line)
		return
	}
	state.cookie = cookie
	// lever's chat page, for both tiers. It claims only GET/HEAD "/" and the
	// /lever prefix, never from the hub. Every other request of a contact
	// goes on to the fence below.
	if g.serveChatPage(w, r, &line, operator, cookie) {
		return
	}
	if names, isContact := cfg.Contacts[operator]; isContact && operator != "" {
		if r = g.fenceContact(w, r, &line, operator, names, &cookie); r == nil {
			return
		}
		state.cookie = cookie
		state.keepDM = contactKeepDM(r)
		state.rewrite = contactRewrite(r)
	}
	// Only a bodiless method may be repeated: the body is consumed by the
	// time forward retries.
	state.retryable = r.Method == http.MethodGet || r.Method == http.MethodHead
	g.forward(w, r, state, operator)
}

// operatorFor is the identity the proxy asserts to the hub: the session
// cache key, and the login the OIDC provider mints a code for. It is the
// identity header's login ONLY when AllowedUsers pins it, because that
// check is the only thing that verifies the header: with the list empty
// the header is an unchecked claim and must not become a hub user row, so
// the hub records the unnamed placeholder operator instead.
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
	// Fail closed on an unconfigured ServeHost, whether or not this request
	// carries an Origin: an empty-string comparison must never decide.
	if cfg.ServeHost == "" {
		g.deny(w, line, http.StatusForbidden, DecisionDenyOrigin, "remote host not configured")
		return false
	}

	// Host first: it is the only gate a header-free request cannot walk
	// through. See hostAllowed, and hostToCheck for which header it reads.
	host, msg := hostToCheck(r, cfg.TrustForwardedHost)
	if msg != "" {
		g.deny(w, line, http.StatusForbidden, DecisionDenyHost, msg)
		return false
	}
	if !hostAllowed(host, cfg.ServeHost, cfg.ListenPort, cfg.BindHost) {
		g.deny(w, line, http.StatusForbidden, DecisionDenyHost, "unexpected Host")
		return false
	}

	if decision, msg := checkOrigin(r, cfg.ServeHost, g.chat != nil); decision != "" {
		if topLevelNavigation(r) {
			g.denyNavigation(w, r, line, http.StatusForbidden, decision, msg)
		} else {
			g.deny(w, line, http.StatusForbidden, decision, msg)
		}
		return false
	}
	if len(cfg.AllowedUsers) > 0 {
		hdr := cfg.identityHeader()
		// Header.Get returns only the FIRST value, so a second one is a
		// header the gate ignores while something downstream might not. A
		// comma-joined value is what a front that APPENDS to a client-sent
		// header produces ("forged, real"), so it is refused too rather
		// than resolved first-value-wins. config.validRemoteLogin keeps
		// commas out of allowed_users.
		if logins := r.Header.Values(hdr); len(logins) > 1 {
			g.deny(w, line, http.StatusForbidden, DecisionDenyUser, "multiple "+hdr+" headers refused")
			return false
		}
		if strings.Contains(login, ",") {
			g.deny(w, line, http.StatusForbidden, DecisionDenyUser, "a comma-joined "+hdr+" value refused")
			return false
		}
		if !slices.Contains(cfg.AllowedUsers, login) {
			g.deny(w, line, http.StatusForbidden, DecisionDenyUser, "identity not allowed")
			return false
		}
	}
	if cfg.Session == nil {
		// No login driver: forwarding with no identity would only earn 401s.
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
	// Only now: a line some later check denies carries no reason.
	line.Reason = navigationReason(r)
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
// request is held back just long enough to learn whether the hub accepted
// the session; if not, the session is replaced and the request repeated
// once.
func (g *gate) forward(w http.ResponseWriter, r *http.Request, state *ctxState, operator string) {
	if !state.retryable {
		r = r.WithContext(context.WithValue(r.Context(), ctxStateKey{}, state))
		g.rp.ServeHTTP(w, r)
		if state.stale {
			g.cfg.Session.Invalidate(operator, state.cookie)
		}
		return
	}

	first := r.WithContext(context.WithValue(r.Context(), ctxStateKey{}, state))
	g.rp.ServeHTTP(&sessionRetryWriter{ResponseWriter: w, state: state}, first)
	if !state.retry {
		return
	}
	// The hub rejected the session. Replace it and answer the request,
	// rather than landing the browser on a login page it cannot complete
	// (see Provider.handleAuthorize).
	g.cfg.Session.Invalidate(operator, state.cookie)
	cookie, err := g.cfg.Session.Cookie(r.Context(), operator)
	if err != nil {
		g.denyNoSession(w, state.line)
		return
	}
	// retryable is not set: one retry, then the hub's answer stands.
	again := &ctxState{line: state.line, cookie: cookie, retried: true, login: state.login, keepDM: state.keepDM, rewrite: state.rewrite}
	g.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxStateKey{}, again)))
}

// sessionRejected reports whether the hub's answer means "I do not know this
// session": a 401 for what scion reads as programmatic, a 302 to its login
// page for what it reads as a browser navigation. A 403 is the hub refusing
// an ACTION, which a new session would not change. A login redirect with
// ?error= is not one either: scion sends those only from its OAuth callback,
// and treating one as "session unknown" would let any client force a fresh
// login per request.
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
// ReverseProxy calls ModifyResponse before it writes anything, so the retry
// decision is made by the time WriteHeader lands here. On a retry the
// status, headers and body are dropped; otherwise this is a pass-through and
// a streamed body still streams.
type sessionRetryWriter struct {
	http.ResponseWriter
	state   *ctxState
	swallow bool
}

func (w *sessionRetryWriter) WriteHeader(status int) {
	if w.state.retry {
		// Clear the hub's rejection headers so the retry writes into a clean
		// response.
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

// Unwrap lets http.ResponseController reach the real writer for deadlines
// and the Hijack that carries the hub's WebSocket upgrades (a 101 is never a
// session rejection).
func (w *sessionRetryWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// hostAllowed reports whether a request asked for a name this proxy answers to.
//
// This is the DNS-rebinding defence, and the ONE check that does not depend
// on the client's goodwill: Origin and Sec-Fetch-Site are sent only when a
// browser chooses to, and loopback is exactly what a rebind targets. A page
// on http://evil.test:8445 whose DNS flips to 127.0.0.1 is SAME-ORIGIN with
// the proxy, sends no Origin and any header it likes, so without this check
// a forged identity header gets a reply with the injected credential's
// authority.
//
// A rebind always makes the browser send the ATTACKER's name in Host, while
// everything legitimate arrives as one of:
//
//   - serveHost, the tailnet name. `tailscale serve` forwards the client's Host
//     unchanged for a TCP backend.
//   - loopback with the proxy's own port, for host-side probes (`lever
//     doctor` dials http://127.0.0.1:<port>/healthz, whatever the bind).
//   - when the listener is bound to a specific address (bindHost: any
//     non-wildcard bind, loopback ones such as 127.0.0.2 included), that
//     address with the proxy's port: what a front sends when it forwards no
//     name. An IP literal is no rebinding target, and whatever can reach
//     that address can already set any header: the non-loopback bind's
//     documented cost.
//
// The outbound Host is rewritten by Rewrite (see newReverseProxy).
func hostAllowed(host, serveHost string, port int, bindHost string) bool {
	if host == "" {
		// HTTP/1.1 requires Host and Go rejects a request without one before
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
	h = strings.ToLower(strings.Trim(h, "[]"))
	switch h {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	if bindHost != "" {
		if want, got := net.ParseIP(bindHost), net.ParseIP(h); want != nil && got != nil && want.Equal(got) && !want.IsUnspecified() {
			return true
		}
	}
	return false
}

// hostToCheck picks the name hostAllowed judges: the request's Host, or,
// only with trustForwarded set and only when the request carries one,
// X-Forwarded-Host. It returns a denial instead when that header is
// ambiguous (more than one, or a comma-joined list: picking one would let a
// client-sent value win), or when the request's own Host is not an IP
// address.
//
// That last rule keeps the DNS-rebinding defence intact: a page rebound onto
// the listener can set X-Forwarded-Host to base_url's host, but its Host is
// always the attacker's NAME, never an IP literal. So the header is believed
// only on a request whose Host is an IP address (what a front dials when it
// rewrites Host). A cross-site page cannot add the header to a request to an
// IP-literal origin either: it is not CORS-safelisted, and the proxy answers
// no preflight.
//
// A request without the header gets the ordinary Host check. A front that
// rewrites Host to a NAME other than base_url's host is not supported.
func hostToCheck(r *http.Request, trustForwarded bool) (host, denial string) {
	if !trustForwarded {
		return r.Host, ""
	}
	xfh := r.Header.Values("X-Forwarded-Host")
	switch {
	case len(xfh) == 0:
		return r.Host, ""
	case len(xfh) > 1:
		return "", "multiple X-Forwarded-Host headers refused"
	case strings.Contains(xfh[0], ","):
		return "", "a comma-joined X-Forwarded-Host value refused"
	case !isIPHost(r.Host):
		return "", "X-Forwarded-Host is trusted only on a request whose Host is an IP address"
	}
	return strings.TrimSpace(xfh[0]), ""
}

// isIPHost reports whether a Host header value is an IP literal, with or
// without a port ("10.0.0.5", "10.0.0.5:8445", "[::1]:8445").
func isIPHost(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if i := strings.IndexByte(h, '%'); i >= 0 {
		return false // a zoned address is no Host a front sends
	}
	return net.ParseIP(h) != nil
}

// tierOf is the chat tier of a verified login: contact when Contacts names
// it, operator otherwise.
func tierOf(contacts map[string][]string, login string) string {
	if _, ok := contacts[login]; ok {
		return chatledger.TierContact
	}
	return chatledger.TierOperator
}
