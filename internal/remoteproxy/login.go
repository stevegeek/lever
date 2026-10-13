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

	"github.com/stevegeek/lever/internal/config"
)

// The server-side login handshake.
//
// The proxy performs every step of scion's OIDC browser flow itself, with its
// own cookie jar; the browser is never redirected anywhere:
//
//  1. GET the hub's /auth/login/oidc and DO NOT follow the 302. The answer
//     carries state, redirect_uri and client_id, and sets the cookie that
//     holds the state.
//  2. Mint a code by calling Provider.Mint, an in-process call. This is why
//     /authorize need not exist.
//  3. GET the hub's own callback with that code and state, carrying the SAME
//     jar. The hub calls the provider (/token, then /userinfo) and answers
//     with a `scion_sess` cookie that is now a session.
//
// The jar must survive from step 1 to step 3: scion stores the login state in
// the cookie that later becomes the session and compares it at the callback,
// so a dropped or swapped jar fails as state_mismatch.
//
// The resulting cookie stays HOST-SIDE: it is injected into the hub-bound half
// of a request and stripped from every response (see proxy.go), so the
// browser never holds a hub credential.

const (
	// sessionCookieName is scion's web session cookie. It carries the login
	// state first and the session afterwards.
	sessionCookieName = "scion_sess"

	// oidcLoginPath starts a login for the generic OIDC provider, and
	// callbackPathPrefix is where the hub expects to be called back
	// (pkg/hub/web.go routes /auth/login/ and /auth/callback/).
	oidcLoginPath      = "/auth/login/oidc"
	callbackPathPrefix = "/auth/callback/"

	// loginTimeout bounds one whole handshake (three short requests) while a
	// browser request waits on the answer.
	loginTimeout = 30 * time.Second

	// sessionMaxAge bounds how long the driver hands out one hub session.
	// scion refuses a scion_sess cookie older than 24 hours and answers as if
	// there were no session; a POST cannot be retried once its body is spent,
	// so the driver renews well before that. The access token INSIDE the
	// cookie lapses after 15 minutes, but the hub refreshes it itself from
	// the refresh token the cookie carries.
	sessionMaxAge = 12 * time.Hour

	// maxLoginBody caps what the driver reads from a hub response: the
	// handshake carries nothing in a body, so this only drains for keep-alive.
	maxLoginBody = 8 << 10
)

type LoginConfig struct {
	// Hub is the hub base URL AS DIALLED (as Config.Target), so the
	// handshake travels the same route as proxied traffic.
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
	hub      *url.URL
	client   *http.Client
	provider *Provider
	audit    func(AuditLine)

	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*sessionEntry
}

// sessionEntry is one login attempt and its result. done is closed once the
// attempt finishes; cookie/err are written before that and read only after,
// so concurrent callers share one attempt.
type sessionEntry struct {
	done   chan struct{}
	cookie string
	err    error
	at     time.Time // when the login succeeded; see sessionMaxAge
}

// NewLoginDriver builds the driver. Its HTTP client carries no jar (each
// attempt gets a fresh one) and never follows a redirect: every hop in this
// handshake is inspected.
func NewLoginDriver(cfg LoginConfig) *LoginDriver {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.DialContext != nil {
		t.DialContext = cfg.DialContext
	}
	// An HTTP_PROXY in the operator's environment must never redirect this
	// handshake off the host.
	t.Proxy = nil
	return &LoginDriver{
		hub:      cfg.Hub,
		client:   &http.Client{Transport: t, CheckRedirect: noRedirect},
		provider: cfg.Provider,
		audit:    cfg.Audit,
		now:      time.Now,
		sessions: map[string]*sessionEntry{},
	}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (d *LoginDriver) hubURL(pathAndQuery string) string {
	return strings.TrimSuffix(d.hub.String(), "/") + pathAndQuery
}

// Cookie returns the session cookie value for the given verified operator
// login, performing the handshake on first use.
//
// The first caller for a login runs the handshake while every other caller
// waits on its result, so a page load's parallel connections drive one
// login. A failed or panicking attempt is NOT cached (see errLoginPanicked):
// the next request retries. A session older than sessionMaxAge is replaced
// by a fresh login.
func (d *LoginDriver) Cookie(ctx context.Context, login string) (string, error) {
	d.mu.Lock()
	if e, ok := d.sessions[login]; ok && !d.expired(e) {
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

	// Every other caller for this login is parked on e.done, so it must be
	// released on EVERY exit path, panic included: net/http recovers a
	// handler panic, and an entry left with its channel open would block
	// every later request for that operator for the life of the process.
	//
	// A panic leaves cookie and err at their zero values, which would read as
	// a session. Name it a failure instead, so waiters get an error and the
	// eviction below lets the next request retry. The panic itself is not
	// recovered: net/http logs it with its stack trace.
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
	e.at = d.now()
	completed = true
	return e.cookie, e.err
}

// expired reports whether a finished, successful attempt is past
// sessionMaxAge. An attempt still running is never expired: its waiters get
// its result. Called with d.mu held; e.at is written before e.done closes.
func (d *LoginDriver) expired(e *sessionEntry) bool {
	select {
	case <-e.done:
		return e.err == nil && d.now().Sub(e.at) >= sessionMaxAge
	default:
		return false
	}
}

// errLoginPanicked is what the waiters on a shared attempt get when the
// caller running it panicked. It says nothing about the panic value: an
// audit line must never carry text this package has not bounded.
var errLoginPanicked = errors.New("remoteproxy: login: the shared login attempt panicked")

// Login refusals the driver decides itself (the hub's own reasons travel
// verbatim in the message). Tests match on these.
var (
	// errForeignProvider: the hub redirected to an authorization endpoint that
	// is not this proxy's, so it is configured against another OIDC provider.
	errForeignProvider = errors.New("remoteproxy: login: the hub is configured against a different OIDC provider")
	// errForeignClientID: the hub asked for a client_id other than LoginClientID.
	// The message is completed in place ("... client_id %q, not lever's %q").
	errForeignClientID = errors.New("remoteproxy: login: the hub asked for client_id")
	// errNoSessionMinted: the callback answered without replacing the
	// login-state cookie with a session.
	errNoSessionMinted = errors.New("remoteproxy: login: the hub answered without turning the login-state cookie into a session")
)

// Invalidate drops a cached session, so the next request logs in again: a
// session the hub no longer accepts heals without operator action.
//
// The cookie value is compared before dropping: a request that started with
// an old session can complete after a concurrent renewal, and must not
// discard the fresh session.
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
// The handshake is detached from the triggering request's cancellation and
// keeps only its own timeout: other requests wait on it (see Cookie), and a
// browser that navigated away must not abort their login.
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
	// scion_sess carries the login state now and becomes the session at the
	// callback. Remember the state value, so a callback that fails without
	// replacing it cannot be mistaken for a login.
	stateCookie := jarCookie(client.Jar, d.hub)
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

type loginStart struct {
	state       string
	redirectURI string
	clientID    string
}

// begin performs step 1 and reads the redirect the hub would have sent a
// browser.
func (d *LoginDriver) begin(ctx context.Context, client *http.Client) (loginStart, error) {
	resp, err := d.get(ctx, client, d.hubURL(oidcLoginPath))
	if err != nil {
		return loginStart{}, fmt.Errorf("remoteproxy: login: start: %w", err)
	}
	defer drain(resp)
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || loc == "" {
		return loginStart{}, fmt.Errorf("remoteproxy: login: the hub answered %s to %s instead of starting an OIDC login — "+
			"is oidc_login configured in the guest ~/.scion/settings.yaml, and was the hub restarted since?", resp.Status, oidcLoginPath)
	}
	// The hub builds this URL from OUR discovery document. Anything else
	// means the hub is configured against a different OIDC provider, and
	// lever must not mint a code for somebody else's login.
	if !strings.HasPrefix(loc, DeadAuthorizationEndpoint) {
		return loginStart{}, fmt.Errorf("%w (it redirected somewhere other than %s)", errForeignProvider, DeadAuthorizationEndpoint)
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
		return loginStart{}, fmt.Errorf("%w %q, not lever's %q — "+
			"the guest ~/.scion/settings.yaml oidc_login block does not match this proxy", errForeignClientID, st.clientID, LoginClientID)
	}
	return st, nil
}

// callback performs step 3 and keeps the session cookie the jar receives.
//
// The redirect_uri is built from the hub's base_url, a name this host need
// not resolve (the proxy reaches the hub through the jail). Only the ORIGIN
// is replaced; the path and query are exactly what the hub asked for, and
// the code stays bound to the redirect_uri as issued, which the hub re-sends
// to /token.
func (d *LoginDriver) callback(ctx context.Context, client *http.Client, redirectURI string, q url.Values, stateCookie string) (string, error) {
	ru, err := url.Parse(redirectURI)
	if err != nil {
		return "", fmt.Errorf("remoteproxy: login: parse the hub's redirect_uri: %w", err)
	}
	if !strings.HasPrefix(ru.Path, callbackPathPrefix) {
		return "", fmt.Errorf("remoteproxy: login: the hub's redirect_uri points at %q, not its own %s callback", ru.Path, callbackPathPrefix)
	}
	resp, err := d.get(ctx, client, d.hubURL(ru.Path+"?"+q.Encode()))
	if err != nil {
		return "", fmt.Errorf("remoteproxy: login: callback: %w", err)
	}
	defer drain(resp)
	// A failed login redirects to the SPA's login page with a reason in the
	// query (state_mismatch, exchange_failed, unauthorized_domain, ...).
	// Report it: it is the hub's own fixed vocabulary.
	if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "/login?") {
		if reason := loginErrorReason(loc); reason != "" {
			return "", fmt.Errorf("remoteproxy: login: the hub refused the callback (%s)", reason)
		}
		return "", fmt.Errorf("remoteproxy: login: the hub refused the callback")
	}
	// A NEW value, not merely a present one: the jar already held this cookie
	// with the login state, so a hub that answers without replacing it would
	// otherwise hand back the state as though it were a session.
	if c := jarCookie(client.Jar, d.hub); c != "" && c != stateCookie {
		return c, nil
	}
	return "", fmt.Errorf("%w (it answered %s and left the %s cookie as it was)", errNoSessionMinted, resp.Status, sessionCookieName)
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
	return truncateAudit(u.Query().Get("error"))
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
// A *url.Error quotes the whole request URL, and the callback leg carries
// the authorization code (the one secret in the flow) in its query, which
// would then reach the audit log and remote.log. The path tells the two legs
// apart; url.Error.Err keeps the cause, which quotes an address, not a URL.
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
		Decision: DecisionOIDCSession,
		Status:   status,
	}
	if err != nil {
		line.Decision, line.Error = DecisionOIDCSessionFailed, err.Error()
	}
	d.audit(line)
}

// unnamedOperator is the identity asserted when the gate hands the driver no
// login: every request while remote.allowed_users is unset. With the
// allowlist empty the identity header is an unverified claim and the gate
// withholds it (see gate.operatorFor), so this is a placeholder for the
// hub's user row, not a claim about who is connected.
const (
	unnamedOperator      = "lever-operator"
	unnamedOperatorEmail = "lever-operator@lever.local"
)

// IDEmailDomain is the domain identityFor gives a login that is not an email
// address (a front's opaque user id, such as exe.dev's X-ExeDev-UserID).
// config.validRemoteLogin keeps this domain (and the unnamed operator's
// address) out of allowed_users, so no listed login can alias another's hub
// user; a test pins that the two packages agree.
const IDEmailDomain = "id.lever.local"

// identityFor turns the login the proxy already verified into the claims the
// hub records. scion keys its user row on the email, so two operators get
// two hub users.
//
// The email is the login itself when it contains "@". A login without one
// becomes <id>@IDEmailDomain, a domain nobody can receive mail at, so it is
// never mistaken for a real address; config.validRemoteLogin restricts such
// ids to characters that make a plain local part. The subject and display
// name stay the login as sent.
func identityFor(login string) Identity {
	if login == "" {
		return Identity{Subject: unnamedOperator, Email: unnamedOperatorEmail, Name: "Lever operator"}
	}
	return Identity{Subject: "lever-remote:" + login, Email: config.HubEmailFor(login), Name: login}
}

// HubUserEmails is the set of hub user emails the proxy's sign-ins can
// create, given remote.allowed_users: one per allowed login, or the single
// placeholder operator when the list is empty. The bootstrap-token step
// binds exactly these to the remote web role.
func HubUserEmails(allowedUsers []string) []string {
	if len(allowedUsers) == 0 {
		return []string{config.HubEmailFor("")}
	}
	out := make([]string, 0, len(allowedUsers))
	for _, l := range allowedUsers {
		out = append(out, identityFor(l).Email)
	}
	return out
}
