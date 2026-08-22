// Package remoteproxy is the host-side seam between the operator's tailnet
// and the jail hub's web UI. Auth is INJECTED here (the phone never holds a
// hub credential), which is exactly why network provenance alone must never
// authenticate a browser-borne cross-site request: any website open on a
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
// injected PAT. Enforcing the loopback bind is the caller's job (see the
// remote-serve CLI wiring). See the 2026-08-16 remote-agent-access design
// spec.
package remoteproxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
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
	// ResponseHeaderTimeout bounds the wait for the hub's response HEADERS.
	// 0 uses DefaultResponseHeaderTimeout; tests lower it. It does not bound
	// the body, so a streamed response and an upgraded connection are
	// unaffected — the timer stops once the headers land. Only meaningful
	// alongside DialContext, which is when this package owns the Transport.
	ResponseHeaderTimeout time.Duration
	// PAT returns the remote PAT at call time (lazy, like HubTokenSource) —
	// a PAT minted mid-apply is picked up without restart. Empty = 503.
	PAT func() string
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
	// Session supplies the hub web session for the UI SHELL — and only the
	// shell. nil disables session injection entirely (API-only proxying,
	// which is what every test that doesn't care about the UI wants).
	//
	// scion's web layer authenticates a browser by cookie alone: it never
	// reads Authorization (pkg/hub/web.go sessionAuthMiddleware), so the
	// injected PAT cannot open the shell. Its API layer is the other way
	// round: sessionToBearerMiddleware passes a request through untouched
	// when it already carries an Authorization header, so /api/v1 keeps
	// riding the NARROW remote PAT. Attaching the session to /api/v1 too
	// would silently widen what the phone can do to whatever the session's
	// hub user may do — which is why the session is never sent there (see
	// isAPIPath, enforced both at the gate and in Rewrite).
	Session SessionSource
	// Audit receives one line per decision; nil disables (tests).
	Audit func(line AuditLine)
}

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

// apiPathPrefix is the hub's REST surface. Everything else the proxy forwards
// — the SPA shell, its assets, /auth/me — is the "web shell" the session
// cookie is for.
const apiPathPrefix = "/api/v1"

// isAPIPath reports whether p is a hub API request rather than a UI-shell one.
// Exact match as well as prefix: "/api/v1" itself is the API, and a path like
// "/api/v1x" is not.
//
// p is the DECODED path (r.URL.Path) and it is not cleaned. A 2026-08-22
// review asked whether that could disagree with scion's own routing and let
// the session cookie ride a request the hub then treats as API. It cannot, at
// pin e82a2a08, where the API and the SPA share one http.ServeMux
// (pkg/hub/web.go: MountHubAPI registers "/api/v1/", the SPA catch-all takes
// "/"). Checked against net/http's ServeMux directly:
//
//   - it unescapes a literal segment before matching, so "/%61pi/v1/agents"
//     does reach the API handler — and reads as API here too, precisely
//     because this sees the decoded path. Matching on the ESCAPED path is what
//     would disagree.
//   - it cleans the escaped path and answers a redirect, rather than
//     dispatching, whenever cleaning changes it. The shapes this function
//     reads as shell — "//api/v1/agents", "/./api/v1/agents" — therefore never
//     reach an API handler at all.
//
// The reverse mismatch exists ("/api/v1%2fagents" reads as API here and is
// routed to the SPA there) and is harmless: it only WITHHOLDS a session.
//
// Were that ever to change, the cookie still could not widen what the phone
// may do: scion's sessionToBearerMiddleware (pkg/hub/web.go) passes a request
// through untouched when it already carries an Authorization header, Rewrite
// always sets one, and nothing else under pkg/hub reads the session cookie.
func isAPIPath(p string) bool {
	return p == apiPathPrefix || strings.HasPrefix(p, apiPathPrefix+"/")
}

// AuditLine is emitted once per request, regardless of outcome. It never
// carries the PAT value.
type AuditLine struct {
	Time    time.Time `json:"time"`
	TSLogin string    `json:"ts_login,omitempty"`
	Method  string    `json:"method"`
	Path    string    `json:"path"`
	// Decision is the outcome. Proxied requests use "allow", "deny-origin",
	// "deny-user", "deny-no-pat" or "deny-no-session"; the login path adds
	// "oidc-session"/"oidc-session-failed" (see login.go) and the provider's
	// own "oidc-discovery", "oidc-token", "oidc-userinfo", their -refused
	// forms, "oidc-not-found", and "deny-authorize" (see oidc.go).
	Decision string `json:"decision"`
	Status   int    `json:"status,omitempty"`
	// Error records why an allowed request never got an answer from the hub
	// (set only on the 502 path). The transport's own diagnosis lands here
	// rather than in the client's response body: the operator needs to know
	// that, say, the jail has no nc, and the remote client must not.
	Error string `json:"error,omitempty"`
}

// maxAuditFieldLen caps how much caller-chosen text any single audit field
// carries. Every field of an AuditLine except Decision and Status is copied
// from the request, and both sinks that write remote-audit.jsonl take input a
// caller controls: the provider is reachable from anything inside the jail,
// and the proxy's own gate audits every DENIAL too, which happens before any
// identity check has passed. JSON encoding already makes an odd value
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

// DefaultResponseHeaderTimeout bounds the wait for the hub's response
// headers when Config leaves it unset. Generous on purpose: it is a
// last-resort guard against a wedged jail, not a latency budget. The hub
// answers a healthy request in milliseconds, and the slowest legitimate case
// — a cold hub still starting inside a machine that just came up — is far
// under this.
const DefaultResponseHeaderTimeout = 45 * time.Second

// secFetchSiteAllowed are the only Sec-Fetch-Site values a same-origin or
// same-site request can carry. Anything else — including values the Fetch
// Metadata spec hasn't defined yet — is refused: an allowlist, not a
// denylist of "cross-site", so an unrecognized value fails closed instead
// of silently passing.
var secFetchSiteAllowed = []string{"same-origin", "same-site", "none"}

// ctxState carries the one-time-read PAT and the in-flight AuditLine from
// the gate (which decides "allow") to the ReverseProxy hooks (which inject
// the PAT and, once the real upstream status is known, complete the audit
// call). Threaded through the request context because Rewrite/ModifyResponse/
// ErrorHandler only see *http.Request/*http.Response, not the gate's locals.
type ctxState struct {
	pat  string
	line *AuditLine
	// cookie is the hub session injected on THIS attempt, empty for API
	// requests and whenever Config.Session is unset.
	cookie string
	// retry is set by ModifyResponse when the hub rejected that session, and
	// read by the gate (to log in again) and by sessionRetryWriter (to
	// swallow the rejection instead of showing the operator a login page).
	// ModifyResponse runs BEFORE ReverseProxy writes anything to the
	// ResponseWriter, which is what makes one flag enough for both.
	retry bool
	// retryable records that this attempt may be repeated: the first attempt
	// of a shell request whose method carries no body. It stops a retry loop
	// (the second attempt sets it false) and keeps a POST from being replayed.
	retryable bool
}

type ctxStateKey struct{}

// NewHandler returns the full middleware+proxy stack.
func NewHandler(cfg Config) http.Handler {
	stateFrom := func(r *http.Request) *ctxState {
		s, _ := r.Context().Value(ctxStateKey{}).(*ctxState)
		return s
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(cfg.Target)
			// Strip any client-supplied identity — the injected PAT is the
			// only identity the hub ever sees. The Cookie header in
			// particular must never reach the hub's cookie→bearer bridge,
			// or a client-supplied scion_sess would be honored as an
			// alternate credential. Tailscale-* headers are stripped too:
			// the AllowedUsers gate trusts them (under the loopback-bind
			// precondition documented above), but the hub must never see a
			// client-supplied identity claim of its own.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			for k := range pr.Out.Header {
				if strings.HasPrefix(strings.ToLower(k), "tailscale-") {
					pr.Out.Header.Del(k)
				}
			}
			var pat, cookie string
			if s := stateFrom(pr.In); s != nil {
				pat, cookie = s.pat, s.cookie
			}
			pr.Out.Header.Set("Authorization", "Bearer "+pat)
			// The hub session opens the UI shell, which the PAT cannot. It is
			// re-checked against the path here as well as at the gate: this is
			// the single funnel every upstream request passes through, so the
			// "an API call never rides the session" property holds even if a
			// future caller populates the cookie somewhere it shouldn't.
			if cookie != "" && !isAPIPath(pr.In.URL.Path) {
				pr.Out.Header.Set("Cookie", sessionCookieName+"="+cookie)
			}
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
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
				if s.line != nil && cfg.Audit != nil {
					s.line.Status = resp.StatusCode
					cfg.Audit(*s.line)
				}
			}
			return nil
		},
	}
	if cfg.DialContext != nil {
		// The standard Transport with only its dial replaced. Not a
		// hand-rolled RoundTripper: httputil.ReverseProxy leans on
		// http.Transport's own 101/upgrade handling to carry the hub's
		// WebSocket attach streams, and that is not part of the RoundTripper
		// contract.
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.DialContext = cfg.DialContext
		// A jail dial has no host-side socket to proxy. Leaving Proxy set
		// would let an HTTP_PROXY in the operator's environment silently
		// redirect hub traffic — carrying the injected PAT — off the host.
		t.Proxy = nil
		// Nothing else bounds getting a response out of the hub. The jail
		// dial returns as soon as the child starts, so a wedged machine or a
		// hung jail transport binary would otherwise hold the request open
		// forever: no answer, no audit line, and a live child — the exact
		// diagnosable-502 this transport exists to produce, lost. This bounds
		// the headers only, so streamed bodies and upgraded connections run
		// as long as they like.
		t.ResponseHeaderTimeout = cfg.ResponseHeaderTimeout
		if t.ResponseHeaderTimeout == 0 {
			t.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
		}
		rp.Transport = t
	}

	// The upstream round trip can fail outright (hub down/unreachable),
	// which bypasses ModifyResponse entirely. Complete the audit call here
	// too, so "exactly one AuditLine per request" holds on this path as
	// well.
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if s := stateFrom(r); s != nil && s.line != nil {
			s.line.Status = http.StatusBadGateway
			if err != nil {
				s.line.Error = err.Error()
			}
			switch {
			case cfg.Audit != nil:
				cfg.Audit(*s.line)
			case err != nil:
				// No audit sink wired: the cause still must not vanish, or a
				// 502 says nothing about whether the jail, the hub, or the
				// PAT is at fault.
				fmt.Fprintf(os.Stderr, "lever: warning: remote proxy upstream: %v\n", err)
			}
		}
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		deny := func(status int, decision, msg string) {
			line.Decision, line.Status = decision, status
			if cfg.Audit != nil {
				cfg.Audit(line)
			}
			http.Error(w, msg, status)
		}

		// Fail closed on an unconfigured ServeHost: it can never
		// legitimately match a request's Origin, so refuse everything
		// rather than let an accidental empty-string comparison decide.
		// Applies regardless of whether this particular request carries an
		// Origin header at all.
		if cfg.ServeHost == "" {
			deny(http.StatusForbidden, "deny-origin", "remote host not configured")
			return
		}

		// Host first: it is the only gate a header-free request cannot walk
		// through. See hostAllowed.
		if !hostAllowed(r.Host, cfg.ServeHost, cfg.ListenPort) {
			deny(http.StatusForbidden, "deny-host", "unexpected Host")
			return
		}

		if origins := r.Header.Values("Origin"); len(origins) > 0 {
			if len(origins) > 1 {
				deny(http.StatusForbidden, "deny-origin", "multiple Origin headers refused")
				return
			}
			u, err := url.Parse(origins[0])
			if err != nil || u.Host == "" || !strings.EqualFold(u.Host, cfg.ServeHost) {
				deny(http.StatusForbidden, "deny-origin", "cross-origin request refused")
				return
			}
		}
		if sfs := r.Header.Values("Sec-Fetch-Site"); len(sfs) > 0 {
			if len(sfs) > 1 {
				deny(http.StatusForbidden, "deny-origin", "multiple Sec-Fetch-Site headers refused")
				return
			}
			if !slices.ContainsFunc(secFetchSiteAllowed, func(v string) bool { return strings.EqualFold(v, sfs[0]) }) {
				deny(http.StatusForbidden, "deny-origin", "cross-site request refused")
				return
			}
		}
		if len(cfg.AllowedUsers) > 0 {
			// Duplicates are refused for the same reason Origin and
			// Sec-Fetch-Site are: Header.Get returns only the FIRST value, so a
			// second one is a header the gate silently ignores while something
			// downstream might not.
			if logins := r.Header.Values("Tailscale-User-Login"); len(logins) > 1 {
				deny(http.StatusForbidden, "deny-user", "multiple Tailscale-User-Login headers refused")
				return
			}
			if !slices.Contains(cfg.AllowedUsers, login) {
				deny(http.StatusForbidden, "deny-user", "tailscale identity not allowed")
				return
			}
		}
		pat := cfg.PAT() // read once; reused below for both the check and the injected header
		if pat == "" {
			deny(http.StatusServiceUnavailable, "deny-no-pat", "remote PAT missing — run `lever apply` to mint it")
			return
		}

		line.Decision = "allow"
		// Status is filled in by ModifyResponse/ErrorHandler once the
		// upstream round trip completes; the audit call happens there too,
		// not here, so the line carries the real status instead of the
		// zero value.
		state := &ctxState{pat: pat, line: &line}

		// The UI shell needs a hub session; /api/v1 must NOT get one (see
		// Config.Session). Obtaining it is lazy — the first shell request of
		// the proxy's life performs the login, and an instance nobody opens a
		// browser at never logs in at all.
		if cfg.Session != nil && !isAPIPath(r.URL.Path) {
			cookie, err := cfg.Session.Cookie(r.Context(), login)
			if err != nil {
				deny(http.StatusBadGateway, "deny-no-session", "hub login failed — see .lever-state/remote.log")
				return
			}
			state.cookie = cookie
			// Only a bodiless method may be repeated: the retry below re-runs
			// the request, and a body has already been consumed by then.
			state.retryable = r.Method == http.MethodGet || r.Method == http.MethodHead
		}

		if !state.retryable {
			r = r.WithContext(context.WithValue(r.Context(), ctxStateKey{}, state))
			rp.ServeHTTP(w, r)
			return
		}

		// Retryable shell request: hold the response back just long enough to
		// learn whether the hub accepted the session. If it did (the normal
		// case), everything streams through untouched.
		first := r.WithContext(context.WithValue(r.Context(), ctxStateKey{}, state))
		rp.ServeHTTP(&sessionRetryWriter{ResponseWriter: w, state: state}, first)
		if !state.retry {
			return
		}
		// The hub rejected the session. Replace it and answer the request
		// properly, rather than letting the operator's browser land on a
		// login page it cannot complete (the login is server-side; the SPA's
		// login button leads to an authorization endpoint that does not
		// resolve, by design — see Provider.handleAuthorize).
		cfg.Session.Invalidate(login, state.cookie)
		cookie, err := cfg.Session.Cookie(r.Context(), login)
		if err != nil {
			deny(http.StatusBadGateway, "deny-no-session", "hub login failed — see .lever-state/remote.log")
			return
		}
		// retryable is deliberately not set: one retry, then the hub's answer
		// stands whatever it is.
		again := &ctxState{pat: pat, line: &line, cookie: cookie}
		rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxStateKey{}, again)))
	})
}

// sessionRejected reports whether the hub's answer means "I do not know this
// session". scion says it two ways, depending on what it took the request for:
// a 401 for anything it reads as programmatic, and a 302 to its login page for
// anything it reads as a browser navigation (pkg/hub/web.go
// sessionAuthMiddleware). Nothing else is treated as a session problem — in
// particular a 403 is the hub refusing an ACTION, which a new session would
// not change.
func sessionRejected(resp *http.Response) bool {
	if resp.StatusCode == http.StatusUnauthorized {
		return true
	}
	if resp.StatusCode != http.StatusFound {
		return false
	}
	loc := resp.Header.Get("Location")
	return strings.HasPrefix(loc, "/auth/login") || strings.HasPrefix(loc, "/login")
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
// a forged Tailscale-User-Login and a reply carrying the injected PAT's
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
// rewritten by the director (see NewHandler).
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
