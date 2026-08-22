package remoteproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The server-side login handshake.
//
// scion's OIDC login is a browser flow, and lever deliberately does not let it
// be one. The proxy performs every step itself, with its own cookie jar, and
// the browser is never redirected anywhere:
//
//  1. GET the hub's /auth/login/oidc and DO NOT follow the 302. The hub's
//     answer carries state, redirect_uri and client_id, and sets the cookie
//     that holds the state.
//  2. Mint a code by calling Provider.Mint — an in-process call, not a request
//     to any endpoint. This is the step a browser flow would perform by
//     visiting /authorize, and it is why /authorize need not exist.
//  3. GET the hub's own callback with that code and state, carrying the SAME
//     jar. The hub calls back to the provider (/token, then /userinfo) and
//     answers with a `scion_sess` cookie that is now a session.
//
// The jar must survive from step 1 to step 3: scion stores the login state in
// the very cookie that later becomes the session (pkg/hub/web.go stores
// `oauthState` in `scion_sess`, then constant-time compares it at the
// callback), so a dropped or swapped jar fails as state_mismatch.
//
// The resulting cookie stays HOST-SIDE. It is injected into the hub-bound half
// of a browser request and stripped from every response (see proxy.go), so the
// phone still holds no hub credential of any kind.

const (
	// sessionCookieName is scion's web session cookie (pkg/hub/web.go
	// webSessionName). It carries the login state first and the session
	// afterwards — one cookie, two jobs.
	sessionCookieName = "scion_sess"

	// oidcLoginPath starts a login for the generic OIDC provider, and
	// callbackPathPrefix is where the hub expects to be called back
	// (pkg/hub/web.go routes /auth/login/ and /auth/callback/).
	oidcLoginPath      = "/auth/login/oidc"
	callbackPathPrefix = "/auth/callback/"

	// loginTimeout bounds one whole handshake. Three short requests to a hub
	// on the other side of the jail transport; well past this it is wedged,
	// and a browser request is waiting on the answer.
	loginTimeout = 30 * time.Second

	// maxLoginBody caps what the driver reads from a hub response. Nothing in
	// the handshake is carried in a body — everything is in headers and
	// cookies — so this exists only to drain politely for keep-alive.
	maxLoginBody = 8 << 10
)

// LoginConfig configures NewLoginDriver.
type LoginConfig struct {
	// Hub is the hub base URL AS DIALLED — the same value Config.Target
	// carries, so the handshake travels the same route as proxied traffic.
	Hub *url.URL
	// DialContext, when non-nil, is how the driver reaches Hub (the jail
	// dial in production; nil uses the default dialer in tests).
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// Provider mints the authorization codes. Required.
	Provider *Provider
	// Audit receives one line per login outcome; nil disables (tests). It
	// never carries the cookie or the code.
	Audit func(line AuditLine)
}

// LoginDriver obtains and caches hub sessions, one per verified operator.
type LoginDriver struct {
	// hub is the hub's base URL with no trailing slash, so a path can be
	// appended to it directly.
	hub string
	// jarURL is the same URL, kept whole for reading the cookie jar.
	jarURL   *url.URL
	client   *http.Client
	provider *Provider
	audit    func(AuditLine)

	mu       sync.Mutex
	sessions map[string]*sessionEntry
}

// sessionEntry is one login attempt and its result. done is closed once the
// attempt finishes; cookie/err are written before that and read only after,
// so concurrent callers share one attempt instead of racing several logins
// against the hub.
type sessionEntry struct {
	done   chan struct{}
	cookie string
	err    error
}

// NewLoginDriver builds the driver. The HTTP client is the driver's own: it
// carries no jar (each attempt gets a fresh one) and never follows a redirect,
// because every hop in this handshake is inspected rather than followed.
func NewLoginDriver(cfg LoginConfig) *LoginDriver {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.DialContext != nil {
		t.DialContext = cfg.DialContext
	}
	// Same reasoning as the proxy's transport: a jail dial has no host-side
	// proxy to traverse, and an HTTP_PROXY in the operator's environment must
	// never redirect this handshake off the host.
	t.Proxy = nil
	return &LoginDriver{
		hub:      strings.TrimSuffix(cfg.Hub.String(), "/"),
		jarURL:   cfg.Hub,
		client:   &http.Client{Transport: t, CheckRedirect: noRedirect},
		provider: cfg.Provider,
		audit:    cfg.Audit,
		sessions: map[string]*sessionEntry{},
	}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Cookie returns the session cookie value for the given verified operator
// login, performing the handshake on first use.
//
// Concurrency: the first caller for a login runs the handshake while every
// other caller waits on its result. A browser opens several connections at
// once and they all arrive before any session exists, so without this they
// would each drive their own login — several hub users' worth of churn for one
// page load. A failed attempt is NOT cached: the entry is dropped so the next
// request retries (a hub that was still starting is the common cause). A
// PANICKING attempt is treated as a failed one — see errLoginPanicked.
func (d *LoginDriver) Cookie(ctx context.Context, login string) (string, error) {
	d.mu.Lock()
	if e, ok := d.sessions[login]; ok {
		d.mu.Unlock()
		select {
		case <-e.done:
			return e.cookie, e.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	e := &sessionEntry{done: make(chan struct{})}
	d.sessions[login] = e
	d.mu.Unlock()

	// The entry is published now, so every other caller for this login is
	// parked on e.done and only this goroutine can release them. That has to
	// happen on EVERY exit path, panic included: the handshake runs inline on
	// a request's own goroutine, net/http recovers a handler panic per
	// connection, and the process therefore survives a panic that leaves this
	// entry in d.sessions with its channel open. Every later request for that
	// operator would then block until its own context expired — for the life
	// of the process, since nothing else ever removes the entry.
	//
	// A panic leaves cookie and err at their zero values, which would hand
	// waiters an empty string and a nil error: a non-session that reads as a
	// session. Name it a failure instead, so waiters get an error and the
	// eviction below lets the next request start a fresh attempt. The panic
	// itself is not recovered — it keeps unwinding to net/http, which logs it
	// with the stack trace that says what actually broke.
	completed := false
	defer func() {
		if !completed {
			e.err = errLoginPanicked
		}
		close(e.done)
		if e.err != nil {
			d.mu.Lock()
			if d.sessions[login] == e {
				delete(d.sessions, login)
			}
			d.mu.Unlock()
		}
	}()

	e.cookie, e.err = d.login(ctx, login)
	completed = true
	return e.cookie, e.err
}

// errLoginPanicked is what the waiters on a shared attempt are given when the
// caller running it panicked. Deliberately says nothing about the panic value:
// it is not this driver's to interpret, it is on its way up the winner's stack
// already, and an audit line must never carry text this package has not
// bounded itself.
var errLoginPanicked = errors.New("remoteproxy: login: the shared login attempt panicked")

// Invalidate drops a cached session, so the next request logs in again. It is
// how a session the hub no longer accepts (it restarted, or the session
// expired) heals without operator action.
//
// The cookie value is compared before dropping: a request that started with an
// old session can complete after a concurrent renewal has already installed a
// new one, and it must not discard that fresh session on the way out.
func (d *LoginDriver) Invalidate(login, cookie string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.sessions[login]
	if !ok {
		return
	}
	select {
	case <-e.done:
		if e.cookie == cookie {
			delete(d.sessions, login)
		}
	default:
		// An attempt is still running; it will install its own result.
	}
}

// login runs the whole handshake and returns the session cookie value.
//
// The handshake is detached from the request that triggered it, keeping only
// its own timeout. One browser request wins the race to start a login and
// every other waits on it (see Cookie), so letting the winner's cancellation
// abort the attempt would let a browser that navigated away take the login out
// from under everyone still waiting. Waiters remain free to give up on their
// own contexts.
func (d *LoginDriver) login(ctx context.Context, operator string) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loginTimeout)
	defer cancel()

	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", fmt.Errorf("remoteproxy: login: cookie jar: %w", err)
	}
	// The jar lives for this attempt only: two logins must never share the
	// state cookie that decides which callback belongs to which attempt.
	client := *d.client
	client.Jar = jar

	start, err := d.begin(ctx, &client)
	if err != nil {
		d.recordLogin(operator, 0, err)
		return "", err
	}
	// scion_sess is ONE cookie doing two jobs: it carries the login state now
	// and becomes the session at the callback. Remember which value the state
	// is, so a callback that fails without replacing it cannot be mistaken for
	// a login (see callback).
	stateCookie := jarCookie(client.Jar, d.jarURL)
	code, err := d.provider.Mint(start.state, start.redirectURI, start.clientID, identityFor(operator))
	if err != nil {
		d.recordLogin(operator, 0, err)
		return "", err
	}
	q, err := d.provider.CallbackQuery(code, start.state)
	if err != nil {
		d.recordLogin(operator, 0, err)
		return "", err
	}
	cookie, err := d.callback(ctx, &client, start.redirectURI, q, stateCookie)
	if err != nil {
		d.recordLogin(operator, 0, err)
		return "", err
	}
	d.recordLogin(operator, http.StatusOK, nil)
	return cookie, nil
}

// loginStart is what the hub's 302 tells us about the login it just began.
type loginStart struct {
	state       string
	redirectURI string
	clientID    string
}

// begin performs step 1: ask the hub to start an OIDC login and read the
// redirect it would have sent a browser.
func (d *LoginDriver) begin(ctx context.Context, client *http.Client) (loginStart, error) {
	resp, err := d.get(ctx, client, d.hub+oidcLoginPath)
	if err != nil {
		return loginStart{}, fmt.Errorf("remoteproxy: login: start: %w", err)
	}
	defer drain(resp)
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || loc == "" {
		return loginStart{}, fmt.Errorf("remoteproxy: login: the hub answered %s to %s instead of starting an OIDC login — "+
			"is oidc_login configured in the guest ~/.scion/settings.yaml, and was the hub restarted since?", resp.Status, oidcLoginPath)
	}
	// The hub builds this URL from OUR discovery document, so it must be the
	// endpoint we advertised. Anything else means the hub is configured
	// against a different OIDC provider, and lever must not mint a code for
	// somebody else's login.
	if !strings.HasPrefix(loc, DeadAuthorizationEndpoint) {
		return loginStart{}, fmt.Errorf("remoteproxy: login: the hub is configured against a different OIDC provider "+
			"(it redirected somewhere other than %s)", DeadAuthorizationEndpoint)
	}
	u, err := url.Parse(loc)
	if err != nil {
		return loginStart{}, fmt.Errorf("remoteproxy: login: parse the hub's redirect: %w", err)
	}
	q := u.Query()
	st := loginStart{state: q.Get("state"), redirectURI: q.Get("redirect_uri"), clientID: q.Get("client_id")}
	if st.state == "" || st.redirectURI == "" {
		return loginStart{}, fmt.Errorf("remoteproxy: login: the hub's redirect carried no state or redirect_uri")
	}
	if st.clientID != LoginClientID {
		return loginStart{}, fmt.Errorf("remoteproxy: login: the hub asked for client_id %q, not lever's %q — "+
			"the guest ~/.scion/settings.yaml oidc_login block does not match this proxy", st.clientID, LoginClientID)
	}
	return st, nil
}

// callback performs step 3: hand the hub its own callback, carrying the code
// and state, and keep the session cookie the jar receives.
//
// The redirect_uri the hub issued is built from ITS base_url, which is a name
// this host need not be able to resolve (lever passes no --base-url, so it is
// the hub's own localhost default, and the proxy reaches the hub through the
// jail). Only the ORIGIN is replaced; the path and the query the callback
// consumes are exactly what the hub asked for, and the code stays bound to the
// redirect_uri as issued, because that is the string the hub re-sends to
// /token.
func (d *LoginDriver) callback(ctx context.Context, client *http.Client, redirectURI string, q url.Values, stateCookie string) (string, error) {
	ru, err := url.Parse(redirectURI)
	if err != nil {
		return "", fmt.Errorf("remoteproxy: login: parse the hub's redirect_uri: %w", err)
	}
	if !strings.HasPrefix(ru.Path, callbackPathPrefix) {
		return "", fmt.Errorf("remoteproxy: login: the hub's redirect_uri points at %q, not its own %s callback", ru.Path, callbackPathPrefix)
	}
	resp, err := d.get(ctx, client, d.hub+ru.Path+"?"+q.Encode())
	if err != nil {
		return "", fmt.Errorf("remoteproxy: login: callback: %w", err)
	}
	defer drain(resp)
	// A failed login is a redirect back to the SPA's login page with a reason
	// in the query (state_mismatch, exchange_failed, unauthorized_domain, …).
	// Report the reason: it is the hub's own fixed vocabulary, and it is the
	// difference between "the provider is unreachable from the guest" and
	// "the jar was dropped".
	if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "/login?") {
		if reason := loginErrorReason(loc); reason != "" {
			return "", fmt.Errorf("remoteproxy: login: the hub refused the callback (%s)", reason)
		}
		return "", fmt.Errorf("remoteproxy: login: the hub refused the callback")
	}
	// A NEW value, not merely a present one: the jar already held this cookie
	// carrying the login state, so a hub that answers without replacing it —
	// a 500 that neither redirects to /login nor writes a session — would
	// otherwise hand back the state as though it were a session.
	if c := jarCookie(client.Jar, d.jarURL); c != "" && c != stateCookie {
		return c, nil
	}
	return "", fmt.Errorf("remoteproxy: login: the hub answered %s without turning the login-state %s cookie into a session",
		resp.Status, sessionCookieName)
}

// jarCookie returns the scion_sess value the jar holds for u, or "".
func jarCookie(jar http.CookieJar, u *url.URL) string {
	for _, c := range jar.Cookies(u) {
		if c.Name == sessionCookieName {
			return c.Value
		}
	}
	return ""
}

// loginErrorReason extracts the hub's `error` query value from a /login
// redirect. Returned into an error message, so it is bounded and stripped of
// anything but the value itself.
func loginErrorReason(location string) string {
	u, err := url.Parse(location)
	if err != nil {
		return ""
	}
	return truncatePath(u.Query().Get("error"))
}

func (d *LoginDriver) get(ctx context.Context, client *http.Client, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		// Not %w, and not rawURL: on a malformed URL the stdlib's error quotes
		// the URL it was given, and one of this handshake's two requests
		// carries the authorization code in its query. See requoted.
		return nil, fmt.Errorf("remoteproxy: login: building the request: %v", redactURL(err))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, requoted(req.URL.Path, err)
	}
	return resp, nil
}

// requoted restates a transport failure WITHOUT the URL that produced it.
//
// http.Client.Do returns a *url.Error, and its message quotes the whole
// request URL. The callback leg of this handshake carries the authorization
// code in that URL's query — the one secret in the entire flow — so the
// stdlib's own error text puts a live code into the caller's error, which the
// driver then writes to the audit log and remote.log. Any wedged jail dial, a
// hub that dies mid-handshake, or the login timeout is enough to trigger it.
//
// The path is what an operator needs in order to tell the two legs apart; the
// query never is. url.Error.Err is kept because it carries the actual cause
// (dial refused, deadline exceeded), which quotes an address, not a URL.
func requoted(path string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s %s: %w", ue.Op, path, ue.Err)
	}
	return err
}

// redactURL strips a query string from an error's text, for the one path where
// the error is not a *url.Error and its content is not otherwise known.
func redactURL(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "?"); i >= 0 {
		return msg[:i] + "?<redacted>"
	}
	return msg
}

// drain reads and closes a response body so the connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxLoginBody))
	_ = resp.Body.Close()
}

// recordLogin audits one login outcome. The cookie value is never part of it;
// err is lever's own message, which quotes no secret.
func (d *LoginDriver) recordLogin(operator string, status int, err error) {
	if d.audit == nil {
		return
	}
	line := AuditLine{
		Time:     time.Now().UTC(),
		TSLogin:  operator,
		Method:   http.MethodGet,
		Path:     oidcLoginPath,
		Decision: "oidc-session",
		Status:   status,
	}
	if err != nil {
		line.Decision, line.Error = "oidc-session-failed", err.Error()
	}
	d.audit(line)
}

// unnamedOperator is the identity asserted when the tailnet front end supplied
// no login at all — which only happens when remote.allowed_users is unset, so
// the proxy has nothing to pin and nothing to assert. It is a placeholder
// identity for the hub's user row, not a claim about who is connected; setting
// allowed_users is what makes the hub's record name a real person.
const (
	unnamedOperator      = "lever-operator"
	unnamedOperatorEmail = "lever-operator@lever.local"
)

// identityFor turns the tailnet login the proxy already verified into the
// claims the hub records. The email is the login itself: scion keys its user
// row on the email, so the hub ends up naming exactly the identity that
// passed the allowed_users gate, and two different operators get two different
// hub users rather than sharing one.
func identityFor(login string) Identity {
	if login == "" {
		return Identity{Subject: unnamedOperator, Email: unnamedOperatorEmail, Name: "Lever operator"}
	}
	return Identity{Subject: "lever-remote:" + login, Email: login, Name: login}
}
