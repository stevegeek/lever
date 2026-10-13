// Package remoteproxy is the host-side seam between the operator's front
// (the tailnet by default) and the jail hub's web UI. Every forwarded request
// carries the verified login's own hub web session, obtained host-side
// (login.go), and nothing the client sent in Authorization or Cookie, so the
// browser never holds a hub credential. The hub narrows that user (the
// lever-remote role); the proxy refuses the routes the hub does not narrow
// (mintsCredential, refusedRoute).
//
// Because the credential is injected, network provenance must never
// authenticate a browser-borne cross-site request: the origin rules are
// security, not CORS hygiene. The only cross-site request admitted is a
// top-level load of an entry page (entryNavigation). An unconfigured
// ServeHost fails closed, and the hub's session cookie is stripped from every
// response.
//
// Precondition: the listener must be reachable only by the authenticating
// front, which sets the identity header (Config.IdentityHeader) and
// OVERWRITES any client value. The header is stripped before forwarding, but
// the AllowedUsers check trusts it; a directly reachable listener (LAN,
// port-forward, DNS rebind) lets any caller forge it. A non-loopback bind
// must be on an address the jail cannot reach, behind the operator's firewall
// (config.validateRemoteBind).
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
	"github.com/stevegeek/lever/internal/hubauth"
)

// Config wires a Handler. All fields required unless noted.
type Config struct {
	// Target is the hub base URL as seen from the jail. With DialContext set,
	// only the Host header and path come from here.
	Target *url.URL
	// DialContext, when non-nil, reaches Target (JailDial in `lever remote
	// serve`). Nil uses the default dialer.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// ServeHost is the public origin's host. An Origin other than
	// https://<ServeHost> is rejected. Empty fails closed (every request
	// refused), since an Origin like "null" parses to an empty host.
	ServeHost string
	// ListenPort is the loopback port bound, so hostAllowed admits a host-side
	// probe on that port only. Zero admits the tailnet name only.
	ListenPort int
	// AllowedUsers, when non-empty, pins IdentityHeader values.
	AllowedUsers []string
	// LoginCurrent reports why a login this proxy was started with is no
	// longer admitted by the access `lever apply` last wrote, or nil; the
	// login is "" when AllowedUsers is empty. It is asked on every request
	// and before every push, so a removal or narrowing takes effect without
	// the hub and before the proxy restarts: the proxy answers the chat page,
	// files, wake and voice from its cached session without asking the hub.
	// Nil admits every login AllowedUsers admits.
	LoginCurrent func(login string) error
	// IdentityHeader carries the front's verified login ("" =
	// DefaultIdentityHeader). It is stripped from every forwarded request. The
	// front must OVERWRITE it; a repeated or comma-joined value is refused.
	IdentityHeader string
	// TrustForwardedHost makes the Host check read X-Forwarded-Host, for a
	// front that rewrites Host. Off by default: any client reaching the
	// listener directly can set it (see hostToCheck).
	TrustForwardedHost bool
	// BindHost is the bound address ("" for a wildcard bind); hostAllowed also
	// admits "<BindHost>:<ListenPort>".
	BindHost string
	// Session supplies the verified login's hub web session, the only
	// credential sent. Nil refuses every request (503).
	//
	// scion turns the cookie into a bearer only when the request carries no
	// Authorization header, so Rewrite strips Authorization to keep the shell
	// and the API the same hub user.
	Session SessionSource
	// Audit receives one line per decision; nil disables (tests).
	Audit func(line AuditLine)
	// ChatLedger, when non-nil, receives one entry per web chat post the hub
	// accepted from a verified login. Never called when AllowedUsers is empty.
	ChatLedger func(chatledger.Entry) error
	// Contacts maps each contact-tier login to the agents it may chat with
	// (contact.go). A login not in the map is an operator.
	Contacts map[string][]string
	// ResolveAgents maps agent names to hub agent ids for the contact fence.
	// Without it every contact request is refused.
	ResolveAgents func(ctx context.Context) (map[string]string, error)
	// ContactSession reports why a contact may not post to the agent now, or
	// nil: the session must have started fresh with the current lever skill,
	// since an older skill may trust a marker a contact can type. Nil refuses
	// every contact post.
	ContactSession func(agent string) error
	// ChatAgent, when set, is the manager's name and turns on lever's chat
	// page (chatpage.go) for every verified login. Needs AgentRecords and
	// AllowedUsers.
	ChatAgent string
	// Workers is every configured worker name in config order.
	Workers []string
	// ContactSee maps a contact login to the agents it may see but not message.
	ContactSee map[string][]string
	// AgentRecords lists the hub agent records by name; nil answers every
	// state "unknown".
	AgentRecords func(ctx context.Context) (map[string]AgentRecord, error)
	// Labels returns the labels by agent name. Optional.
	Labels func() map[string]string
	// Wake asks the broker to resume a worker for a login that may message it.
	// A refusal is a *WakeError. Nil answers every wake 503.
	Wake func(ctx context.Context, login, tier, worker string) error
	// MatchAgentMessages reports which agent rows of a contact's DM the
	// broker's agent ledger recorded. A contact sees only those agent rows;
	// any error hides every agent row. Nil: no contact answer is rewritten.
	MatchAgentMessages func(ctx context.Context, contact, agent string, msgs []AgentMessage) (map[string]bool, error)
	// ContactUser reports the hub user id `lever apply` bound a contact login
	// to, or false. The operator view reads only a bound contact's
	// conversations, and never logs in an unbound one, since that would create
	// the contact's hub user.
	ContactUser func(login string) (string, bool)
	// PeekAgentMessages is MatchAgentMessages without binding, so an
	// operator's read never decides which message a record shows. pending is
	// the kept ids no contact read has bound yet. Nil while MatchAgentMessages
	// is set: every answer is unmatched.
	PeekAgentMessages func(ctx context.Context, contact, agent string, msgs []AgentMessage) (keep, pending map[string]bool, err error)
	// Files turns on the chat page's file exchange (files.go). Nil: every
	// /lever/api/files/ path is a 404.
	Files *FilesConfig
	// Voice turns on dictation (voice.go) and, with Voice.Speech, read-aloud
	// (speak.go). Nil: the routes are a 404.
	Voice *VoiceConfig
	// Push is the Web Push service (push.go); the caller runs Push.Run. Needs
	// ChatAgent.
	Push *Push
	// LogPath is the proxy log named in the hub-login-failed denial ("" =
	// DefaultLogPath).
	LogPath string
}

// DefaultIdentityHeader is Config.IdentityHeader when unset: the header
// `tailscale serve` puts the tailnet login in.
const DefaultIdentityHeader = "Tailscale-User-Login"

func (c Config) identityHeader() string {
	return http.CanonicalHeaderKey(cmp.Or(c.IdentityHeader, DefaultIdentityHeader))
}

// DefaultLogPath is the proxy log, relative to the instance root.
const DefaultLogPath = ".lever-state/remote.log"

// SessionSource hands out hub web sessions per verified login.
type SessionSource interface {
	// Cookie returns the scion_sess value for a login, logging in on first
	// use. Concurrent callers must share one attempt.
	Cookie(ctx context.Context, login string) (string, error)
	// Invalidate drops a session the hub rejected. The cookie names which
	// one, so a late request cannot discard a session a renewal just installed.
	Invalidate(login, cookie string)
}

const authAPIPrefix = "/api/v1/auth/"

// mintsCredential reports whether a request under /api/v1/auth/ is anything
// but the few web-UI calls that hand out nothing. It is an allowlist because
// that surface is where scion mints credentials (tokens, CLI device flow,
// Google exchange, invite redeem...): a new minting route must fail closed,
// and a token in a response body leaves the host as surely as a cookie.
//
// p is the decoded path, which is what scion's ServeMux matches; paths that
// cleaning would change get a redirect, not a dispatch.
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

// refusedRoute reports whether a request names a hub route the hub's own
// authorization does not narrow for the injected session:
//
//   - /api/v1/system/*: no RBAC, and assertLoopback passes because the proxy
//     reaches the hub from loopback in the jail. Only GET status is
//     forwarded; unknown routes fail closed.
//   - Project creation (and register, clone, the /groves aliases): each makes
//     the caller the new project's owner, and a first-time user holds
//     project.create until the next `lever apply`, so these are refused
//     whatever the hub's state.
//   - /auth/callback/*: no lever login uses it, and through the proxy it could
//     only churn logins.
//
// p is the decoded path.
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

const callbackPath = "/auth/callback"

// loginPathPrefix is the hub's login route, bare or /auth/login/<provider>.
const loginPathPrefix = "/auth/login"

// isLoginPath excludes the callback route, which refusedRoute refuses.
func isLoginPath(p string) bool {
	return p == loginPathPrefix || strings.HasPrefix(p, loginPathPrefix+"/")
}

// loginReturnTarget picks the redirect after an intercepted sign-in. The
// value is caller-chosen: only an in-app absolute path passes, else "/"
// ("//evil.test" and "/\evil.test" would make an open redirect).
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

// Decision is an AuditLine outcome.
type Decision string

const (
	DecisionAllow      Decision = "allow"
	DecisionDenyHost   Decision = "deny-host"
	DecisionDenyOrigin Decision = "deny-origin"
	DecisionDenyUser   Decision = "deny-user"
	// DecisionDenyRevoked: the login's access changed since the proxy started
	// (Config.LoginCurrent).
	DecisionDenyRevoked      Decision = "deny-revoked"
	DecisionDenyMint         Decision = "deny-credential-mint"
	DecisionDenyRoute        Decision = "deny-route"
	DecisionDenyNoSession    Decision = "deny-no-session"
	DecisionLoginRedirect    Decision = "login-redirect"
	DecisionHubSession       Decision = "hub-session"
	DecisionHubSessionFailed Decision = "hub-session-failed"
	// DecisionHubSessionRefused: the login has no recorded hub user, or the
	// hub's session belongs to another one.
	DecisionHubSessionRefused Decision = "hub-session-refused"
)

// AuditLine is emitted once per request. It never carries the session value.
type AuditLine struct {
	Time time.Time `json:"time"`
	// TSLogin is the identity header's value. The name and JSON key are kept
	// for existing audit readers.
	TSLogin  string   `json:"ts_login,omitempty"`
	Method   string   `json:"method"`
	Path     string   `json:"path"`
	Decision Decision `json:"decision"`
	Status   int      `json:"status,omitempty"`
	// Reason is a fixed word (a wake outcome, or an admitted navigation);
	// never caller or broker text.
	Reason string `json:"reason,omitempty"`
	// Contact, Agent and Count describe an operator-view read. Never message
	// text.
	Contact string `json:"contact,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Count   *int   `json:"count,omitempty"`
	// AudioSeconds and LatencyMS describe a dictation clip. Never the audio
	// or text.
	AudioSeconds float64 `json:"audio_seconds,omitempty"`
	LatencyMS    int64   `json:"latency_ms,omitempty"`
	// MessageID and Chars describe a read-aloud request. Never the text.
	MessageID string `json:"message_id,omitempty"`
	Chars     int    `json:"chars,omitempty"`
	// Error is why an allowed request got no hub answer (502); the
	// transport's diagnosis goes here, not to the client.
	Error string `json:"error,omitempty"`
}

// maxAuditFieldLen caps caller-chosen text in one audit field: the gate
// audits denials before any identity check, and a megabyte of headers per
// request would otherwise fill the operator's disk.
const maxAuditFieldLen = 200

// truncateAudit bounds one audit field. Callers audit the truncated copy but
// DECIDE on the original: truncated logins sharing a prefix would collide.
func truncateAudit(v string) string {
	if len(v) <= maxAuditFieldLen {
		return v
	}
	return v[:maxAuditFieldLen] + "…"
}

// responseHeaderTimeout is a last-resort guard against a wedged jail, not a
// latency budget.
const responseHeaderTimeout = 45 * time.Second

// secFetchSiteAllowed is an allowlist: unknown values fail closed.
// "same-site" is refused because a sibling ts.net name is same-site.
func secFetchSiteAllowed(v string) bool {
	switch strings.ToLower(v) {
	case "same-origin", "none":
		return true
	}
	return false
}

// entryNavigation admits the one cross-site exception: a top-level GET/HEAD
// page load of an entry page (a link from elsewhere, or a front's login
// redirect), with no Origin header and Sec-Fetch-Mode navigate /
// Sec-Fetch-Dest document. Entry pages change nothing on GET and forbid
// framing, and the page's later requests meet the strict rule.
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

func topLevelNavigation(r *http.Request) bool {
	mode, dest := r.Header.Values("Sec-Fetch-Mode"), r.Header.Values("Sec-Fetch-Dest")
	return len(mode) == 1 && strings.EqualFold(mode[0], "navigate") &&
		len(dest) == 1 && strings.EqualFold(dest[0], "document")
}

// entryPage: "/" and, when on, the chat page. p is the decoded path.
func entryPage(p string, chatPage bool) bool {
	return p == "/" || (chatPage && p == chatPagePath)
}

const (
	ReasonCrossSiteNavigation = "cross-site-navigation"
	ReasonSameSiteNavigation  = "same-site-navigation"
)

func navigationReason(r *http.Request) string {
	switch strings.ToLower(r.Header.Get("Sec-Fetch-Site")) {
	case "cross-site":
		return ReasonCrossSiteNavigation
	case "same-site":
		return ReasonSameSiteNavigation
	}
	return ""
}

// denyFraming forbids framing an entry page without relying on the hub's
// X-Frame-Options. A second CSP header only narrows the hub's.
func denyFraming(resp *http.Response) {
	if resp.Request == nil || !entryPage(resp.Request.URL.Path, true) {
		return
	}
	resp.Header.Add("Content-Security-Policy", "frame-ancestors 'none'")
	resp.Header.Set("X-Frame-Options", "DENY")
}

// ctxState carries the session and audit line from the gate to the
// ReverseProxy hooks.
type ctxState struct {
	line   *AuditLine
	cookie string
	// retry: the hub rejected the session. ModifyResponse runs before
	// ReverseProxy writes anything, so one flag serves the gate and
	// sessionRetryWriter.
	retry bool
	// retryable: first attempt of a bodiless request; a POST is never replayed.
	retryable bool
	// stale: rejected on a non-repeatable attempt; the gate drops the session.
	stale bool
	// retried: a just-minted session is not stale.
	retried bool
	// login is "" when AllowedUsers is empty and nothing verified it.
	login string
	// keepDM filters /api/v1/chat/dms for the contact fence.
	keepDM func(key string) bool
	// rewrite rewrites a contact's answer, after keepDM, on every attempt.
	rewrite func(*http.Response)
}

type ctxStateKey struct{}

func stateFrom(r *http.Request) *ctxState {
	s, _ := r.Context().Value(ctxStateKey{}).(*ctxState)
	return s
}

// NewHandler returns the gate in front of the reverse proxy.
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

func rewriteUpstream(target *url.URL, identityHeader string) func(*httputil.ProxyRequest) {
	return func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)
		// The injected session is the only identity the hub sees. scion trusts
		// any Authorization header, and would honor a client scion_sess. The
		// identity header and Tailscale-* are for the gate, never the hub.
		pr.Out.Header.Del("Authorization")
		pr.Out.Header.Del("Cookie")
		pr.Out.Header.Del(identityHeader)
		for k := range pr.Out.Header {
			if clientIdentityHeader(k) {
				// Del would canonicalize k and miss a non-canonical key, which
				// the hub canonicalizes on receipt.
				delete(pr.Out.Header, k)
			}
		}
		if s := stateFrom(pr.In); s != nil && s.cookie != "" {
			pr.Out.Header.Set("Cookie", sessionCookieName+"="+s.cookie)
		}
		pr.SetXForwarded()
	}
}

// clientIdentityHeader reports headers scion's auth middleware reads as
// identity or credentials (besides Authorization and Cookie): never the
// client's to send. The hub trusts hubauth.Header as an admin-grade
// assertion on any route, so a client copy must never reach it.
func clientIdentityHeader(k string) bool {
	k = strings.ToLower(k)
	switch {
	case strings.HasPrefix(k, "tailscale-"),
		strings.HasPrefix(k, "x-scion-"),
		strings.HasPrefix(k, "x-forwarded-user-"):
		return true
	}
	switch k {
	case "x-goog-iap-jwt-assertion", "x-api-key", strings.ToLower(hubauth.Header):
		return true
	}
	return false
}

func completeAudit(audit func(AuditLine), ledger func(chatledger.Entry) error, contacts map[string][]string) func(*http.Response) error {
	return func(resp *http.Response) error {
		// The client must never hold a hub credential, including the fresh
		// cookie the hub mints on every cookie-less request.
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
				// The gate retries with a new session; that attempt is audited.
				s.retry = true
				return nil
			}
			if !s.retried && sessionRejected(resp) {
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

// failOtherSuccess makes a non-200 2xx a bodiless 502: the contact filters
// read only a 200, so another success must not pass the hub's body unread.
func failOtherSuccess(resp *http.Response) {
	if resp.StatusCode == http.StatusOK || resp.StatusCode < 200 || resp.StatusCode > 299 {
		return
	}
	_ = resp.Body.Close()
	resp.StatusCode, resp.Status = http.StatusBadGateway, "502 Bad Gateway"
	resp.Header.Del("Content-Type")
	setBody(resp, nil)
}

// sandboxAPIDocument puts every /api/ response in a CSP sandbox. The hub
// serves agent-written files inline with script allowed; opened as a page
// here they would run on the SPA's origin with the injected session. A
// sandboxed document has an opaque origin ("Origin: null", refused) and runs
// no script. fetch ignores a response CSP. nosniff stops JSON being read as
// HTML.
func sandboxAPIDocument(resp *http.Response) {
	if resp.Request == nil || !strings.HasPrefix(resp.Request.URL.Path, "/api/") {
		return
	}
	resp.Header.Add("Content-Security-Policy", "sandbox")
	resp.Header.Set("X-Content-Type-Options", "nosniff")
}

// upstreamFailed completes the audit line when the round trip failed and
// ModifyResponse never ran.
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
				slog.Warn("upstream request failed", "err", err)
			}
		}
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}
}

// jailTransport is http.Transport with the dial replaced, not a custom
// RoundTripper: ReverseProxy needs its upgrade handling for WebSockets.
func jailTransport(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = dial
	// An HTTP_PROXY must never send hub traffic, with the session, off the host.
	t.Proxy = nil
	// The jail dial returns once the child starts, so a wedged machine would
	// otherwise hold the request forever. Bodies and upgrades run on.
	t.ResponseHeaderTimeout = responseHeaderTimeout
	return t
}

// checkOrigin applies the browser-provenance rules: at most one Origin,
// equal to https://<serveHost>; at most one Sec-Fetch-Site, same-origin or
// none unless entryNavigation admits it. A request with neither header
// passes; hostAllowed covers that case.
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
	live        liveRequests
	// revokeEvery overrides revokePoll (tests).
	revokeEvery time.Duration
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

// denyNavigation is deny answered as a small HTML page (a phone may offer
// text/plain as a download). No script; msg is escaped anyway.
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

func refusalPage(msg string) string {
	return `<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width, initial-scale=1"><title>Refused</title></head>` +
		`<body><p>` + html.EscapeString(msg) + `</p><p><a href="/">Open the chat</a></p></body></html>` + "\n"
}

func (g *gate) denyNoSession(w http.ResponseWriter, line *AuditLine) {
	g.deny(w, line, http.StatusBadGateway, DecisionDenyNoSession,
		"hub login failed — see "+cmp.Or(g.cfg.LogPath, DefaultLogPath))
}

func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := g.cfg
	// Decide on the whole login; audit only bounded copies of caller text.
	login := r.Header.Get(cfg.identityHeader())
	line := AuditLine{Time: time.Now().UTC(), TSLogin: truncateAudit(login), Method: truncateAudit(r.Method), Path: truncateAudit(r.URL.Path)}
	if msg := unsafePath(r); msg != "" {
		g.deny(w, &line, http.StatusBadRequest, DecisionDenyPath, msg)
		return
	}

	if !g.authorize(w, r, &line, login) {
		return
	}
	// operatorFor: the header is verified only when AllowedUsers pins it.
	operator := g.operatorFor(login)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer g.track(operator, cancel)()
	r = r.WithContext(ctx)
	line.Decision = DecisionAllow
	// ModifyResponse/ErrorHandler set Status and audit.
	state := &ctxState{line: &line, login: operator}

	// A sign-in navigation is answered here, never forwarded: the session
	// comes from the login driver, not the hub's login page. The driver dials
	// the hub directly, so it cannot recurse here.
	if isLoginPath(r.URL.Path) && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		g.serveLogin(w, r, &line, operator)
		return
	}

	cookie, err := cfg.Session.Cookie(r.Context(), operator)
	if err != nil {
		g.denyNoSession(w, &line)
		return
	}
	state.cookie = cookie
	// The chat page claims only GET/HEAD "/" and /lever; a contact's other
	// requests go to the fence.
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
	// The body is consumed by the time forward retries.
	state.retryable = r.Method == http.MethodGet || r.Method == http.MethodHead
	g.forward(w, r, state, operator)
}

// operatorFor is the identity asserted to the hub (session cache key, hub
// login). The header counts only when AllowedUsers pins it: unverified, it
// must not become a hub user row, so the hub gets the unnamed operator.
// Called after authorize.
func (g *gate) operatorFor(login string) string {
	if len(g.cfg.AllowedUsers) == 0 {
		return ""
	}
	return login
}

// loginCurrent fails closed on any error from Config.LoginCurrent.
func (g *gate) loginCurrent(login string) bool {
	return g.cfg.LoginCurrent == nil || g.cfg.LoginCurrent(login) == nil
}

// authorize runs every check that decides whether a request may reach the
// hub, denying and auditing on the first failure.
func (g *gate) authorize(w http.ResponseWriter, r *http.Request, line *AuditLine, login string) bool {
	cfg := g.cfg
	// An empty-string comparison must never decide.
	if cfg.ServeHost == "" {
		g.deny(w, line, http.StatusForbidden, DecisionDenyOrigin, "remote host not configured")
		return false
	}

	// Host first: the only check a header-free request cannot skip.
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
		// A second value is one the gate ignores and something downstream
		// might not; a comma-joined value is what an APPENDING front produces.
		// config.validRemoteLogin keeps commas out of allowed_users.
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
	if !g.loginCurrent(g.operatorFor(login)) {
		g.deny(w, line, http.StatusForbidden, DecisionDenyRevoked, "this login's access changed; it is refused until `lever apply` completes")
		return false
	}
	if cfg.Session == nil {
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
	// Set last, so a denied line carries no reason.
	line.Reason = navigationReason(r)
	return true
}

func (g *gate) serveLogin(w http.ResponseWriter, r *http.Request, line *AuditLine, operator string) {
	if _, err := g.cfg.Session.Cookie(r.Context(), operator); err != nil {
		g.denyNoSession(w, line)
		return
	}
	line.Decision, line.Status = DecisionLoginRedirect, http.StatusFound
	g.audit(*line)
	http.Redirect(w, r, loginReturnTarget(r.URL.Query().Get("returnTo")), http.StatusFound)
}

// forward retries a bodiless request once with a new session when the hub
// rejects the first.
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
	// Replace the session rather than land the browser on a login page it
	// cannot complete.
	g.cfg.Session.Invalidate(operator, state.cookie)
	cookie, err := g.cfg.Session.Cookie(r.Context(), operator)
	if err != nil {
		g.denyNoSession(w, state.line)
		return
	}
	// retryable stays false: one retry, then the hub's answer stands.
	again := &ctxState{line: state.line, cookie: cookie, retried: true, login: state.login, keepDM: state.keepDM, rewrite: state.rewrite}
	g.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxStateKey{}, again)))
}

// sessionRejected reports whether the hub means "unknown session": 401, or
// a 302 to its login page. A 403 refuses an action, which a new session
// would not change. A login redirect with ?error= comes only from scion's
// OAuth callback; counting it would let any client force a login per
// request.
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
// ModifyResponse has decided the retry before WriteHeader lands here.
type sessionRetryWriter struct {
	http.ResponseWriter
	state   *ctxState
	swallow bool
}

func (w *sessionRetryWriter) WriteHeader(status int) {
	if w.state.retry {
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

// Unwrap lets ResponseController reach Hijack for the hub's WebSocket
// upgrades (a 101 is never a session rejection).
func (w *sessionRetryWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// hostAllowed is the DNS-rebinding defence, the one check that does not
// depend on the client: a rebound page is same-origin with the proxy and
// sends any header it likes, but its Host is always the attacker's name.
// Legitimate Hosts are serveHost (`tailscale serve` forwards the client's
// Host), loopback with the proxy's port (host-side probes), and bindHost
// with the port for a specific bind (an IP literal is no rebinding target).
func hostAllowed(host, serveHost string, port int, bindHost string) bool {
	if host == "" {
		// Go rejects a request without Host; treat it as hostile.
		return false
	}
	if strings.EqualFold(host, serveHost) {
		return true
	}
	h, p, err := net.SplitHostPort(host)
	if err != nil {
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

// hostToCheck picks the name hostAllowed judges: Host, or with
// trustForwarded, X-Forwarded-Host when present. An ambiguous header is
// refused, and the header is believed only when Host is an IP literal: a
// rebound page can set it, but its Host is always a name, and a cross-site
// page cannot add it to an IP origin (not CORS-safelisted, no preflight
// answered). A front that rewrites Host to some other name is unsupported.
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

func tierOf(contacts map[string][]string, login string) string {
	if _, ok := contacts[login]; ok {
		return chatledger.TierContact
	}
	return chatledger.TierOperator
}
