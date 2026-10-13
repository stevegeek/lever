package remoteproxy

import (
	"context"
	"encoding/json"
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

// The server-side login handshake. The proxy drives scion's OIDC browser flow
// itself with a per-attempt cookie jar; the browser is never redirected:
//
//  1. GET the hub's /auth/login/oidc without following the 302, which yields
//     state, redirect_uri and client_id and sets the state cookie.
//  2. Mint a code in-process with Provider.Mint (so /authorize need not exist).
//  3. GET the hub's callback with that code and state using the SAME jar; the
//     hub answers with a scion_sess cookie that is now a session.
//
// scion keeps the login state in the cookie that becomes the session, so a
// dropped or swapped jar fails as state_mismatch. The session cookie stays
// host-side: proxy.go injects it hub-bound and strips it from every response,
// so the browser never holds a hub credential.

const (
	// sessionCookieName carries the login state first and the session after.
	sessionCookieName = "scion_sess"

	// pkg/hub/web.go routes /auth/login/ and /auth/callback/.
	oidcLoginPath      = "/auth/login/oidc"
	callbackPathPrefix = "/auth/callback/"

	loginTimeout = 30 * time.Second

	// scion refuses a scion_sess older than 24 hours as if there were no
	// session, and a spent POST body cannot be retried, so renew well before.
	// The hub refreshes the 15-minute access token inside the cookie itself.
	sessionMaxAge = 12 * time.Hour

	// refusalBackoff is how long a login whose session resolved to a hub user
	// lever will not serve stays refused before it is tried again.
	refusalBackoff = time.Minute

	// maxLoginBody only drains for keep-alive: the handshake has no body.
	maxLoginBody = 8 << 10
)

type LoginConfig struct {
	// Hub is the hub base URL as dialled (as Config.Target).
	Hub *url.URL
	// DialContext reaches Hub (the jail dial); nil uses the default dialer.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	Provider    *Provider
	// Audit receives one line per login outcome; nil disables. It never
	// carries the cookie or the code.
	Audit func(line AuditLine)
	// BoundUser reports the hub user id `lever apply` recorded for a login
	// (remote-role.json), or false when none is recorded yet. The driver
	// refuses a session that resolves to a different id, so a renamed or
	// swapped hub row cannot inherit a login's access. Nil disables that
	// second rule; the dev-user rule below always holds.
	BoundUser func(login string) (string, bool)
}

// LoginDriver obtains and caches hub sessions, one per verified operator.
type LoginDriver struct {
	hub      *url.URL
	client   *http.Client
	provider *Provider
	audit    func(AuditLine)
	bound    func(login string) (string, bool)

	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*sessionEntry
}

// sessionEntry is one login attempt shared by concurrent callers: cookie, err
// and at are written before done closes and read only after.
type sessionEntry struct {
	done   chan struct{}
	cookie string
	err    error
	at     time.Time // when the login succeeded; see sessionMaxAge
}

// NewLoginDriver builds the driver. Its client has no jar (each attempt gets
// its own) and never follows a redirect: every hop is inspected.
func NewLoginDriver(cfg LoginConfig) *LoginDriver {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.DialContext != nil {
		t.DialContext = cfg.DialContext
	}
	// An operator's HTTP_PROXY must never route this handshake off the host.
	t.Proxy = nil
	return &LoginDriver{
		hub:      cfg.Hub,
		client:   &http.Client{Transport: t, CheckRedirect: noRedirect},
		provider: cfg.Provider,
		audit:    cfg.Audit,
		bound:    cfg.BoundUser,
		now:      time.Now,
		sessions: map[string]*sessionEntry{},
	}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (d *LoginDriver) hubURL(pathAndQuery string) string {
	return strings.TrimSuffix(d.hub.String(), "/") + pathAndQuery
}

// Cookie returns the hub session cookie for a verified operator login,
// logging in on first use. Concurrent callers share one attempt; a failed or
// panicking attempt is not cached, and a session older than sessionMaxAge is
// replaced.
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

	// Waiters are parked on e.done, so it must close on every exit path, panic
	// included, or that operator's requests block for the life of the process.
	// A panic leaves zero values that would read as a session, so mark it a
	// failure; the panic itself is left for net/http to log.
	completed := false
	defer func() {
		if !completed {
			e.err = errLoginPanicked
		}
		close(e.done)
		// A refused hub user is kept for refusalBackoff: every retry is a full
		// sign-in that scion's callback applies to the row it lands on.
		if e.err != nil && !errors.Is(e.err, errForeignHubUser) {
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
// sessionMaxAge; a running attempt is never expired. Called with d.mu held.
func (d *LoginDriver) expired(e *sessionEntry) bool {
	select {
	case <-e.done:
		if errors.Is(e.err, errForeignHubUser) {
			return d.now().Sub(e.at) >= refusalBackoff
		}
		return e.err == nil && d.now().Sub(e.at) >= sessionMaxAge
	default:
		return false
	}
}

// errLoginPanicked says nothing about the panic value: an audit line must
// never carry text this package has not bounded.
var errLoginPanicked = errors.New("remoteproxy: login: the shared login attempt panicked")

// Tests match on these.
var (
	errForeignProvider = errors.New("remoteproxy: login: the hub is configured against a different OIDC provider")
	// errForeignClientID is completed in place ("... client_id %q, not lever's %q").
	errForeignClientID = errors.New("remoteproxy: login: the hub asked for client_id")
	errNoSessionMinted = errors.New("remoteproxy: login: the hub answered without turning the login-state cookie into a session")
)

// Invalidate drops a cached session so the next request logs in again. It
// compares the cookie first: a request that started on an old session must
// not discard a concurrent renewal.
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
	}
}

// login is detached from the triggering request's cancellation (keeping its
// own timeout): other requests wait on it and must not fail because one
// browser navigated away.
func (d *LoginDriver) login(ctx context.Context, operator string) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loginTimeout)
	defer cancel()

	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", fmt.Errorf("remoteproxy: login: cookie jar: %w", err)
	}
	// Two logins must never share the state cookie that ties a callback to
	// its attempt.
	client := *d.client
	client.Jar = jar

	start, err := d.begin(ctx, &client)
	if err != nil {
		d.recordLogin(operator, 0, err)
		return "", err
	}
	// Remember the state value so a callback that fails without replacing it
	// is not mistaken for a session.
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
	// The session now names a hub user. Refuse it before any request rides it
	// if it is not the user lever expects for this login: never scion's dev
	// super-admin row (which any jailed agent can rename to this login's email
	// through scion's workstation identity route), and, when lever recorded an
	// id for the login, exactly that id.
	if err := d.verifyUser(ctx, &client, operator, cookie); err != nil {
		d.recordLogin(operator, 0, err)
		return "", err
	}
	d.recordLogin(operator, http.StatusOK, nil)
	return cookie, nil
}

// errForeignHubUser is a session that resolved to a hub user lever will not
// serve for this login.
var errForeignHubUser = errors.New("remoteproxy: login: the session resolved to a hub user lever did not bind to this login")

// verifyUser reads the freshly minted session's hub user id and applies the
// two rules. A hub it cannot reach, or an unreadable answer, fails the login
// closed.
func (d *LoginDriver) verifyUser(ctx context.Context, client *http.Client, operator, cookie string) error {
	id, err := d.whoAmI(ctx, client, cookie)
	if err != nil {
		return fmt.Errorf("remoteproxy: login: verifying the session's hub user: %w", err)
	}
	if id == "" || !validHubID(id) || id == config.ScionDevUserID {
		return errForeignHubUser
	}
	if d.bound != nil {
		if want, ok := d.bound(operator); ok && want != id {
			return errForeignHubUser
		}
	}
	return nil
}

// whoAmI reads GET /api/v1/auth/me with the session cookie and returns the
// hub user id.
func (d *LoginDriver) whoAmI(ctx context.Context, client *http.Client, cookie string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.hubURL("/api/v1/auth/me"), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cookie", sessionCookieName+"="+cookie)
	// The handshake jar holds the same cookie; send only the explicit one.
	plain := *client
	plain.Jar = nil
	resp, err := plain.Do(req)
	if err != nil {
		return "", err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /api/v1/auth/me: HTTP %d", resp.StatusCode)
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxLoginBody)).Decode(&body); err != nil {
		return "", fmt.Errorf("GET /api/v1/auth/me: %w", err)
	}
	return body.ID, nil
}

type loginStart struct {
	state       string
	redirectURI string
	clientID    string
}

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
	// The hub builds this URL from our discovery document; anything else means
	// another OIDC provider, and lever must not mint a code for its login.
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

// callback performs step 3. Only the redirect_uri's origin is replaced (the
// hub's base_url need not resolve here); path and query stay as issued, since
// the hub re-sends the redirect_uri to /token.
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
	// A failed login redirects to /login?error=<reason>, the hub's own fixed
	// vocabulary, so the reason is safe to report.
	if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "/login?") {
		if reason := loginErrorReason(loc); reason != "" {
			return "", fmt.Errorf("remoteproxy: login: the hub refused the callback (%s)", reason)
		}
		return "", fmt.Errorf("remoteproxy: login: the hub refused the callback")
	}
	// The jar already held this cookie with the login state: require a new
	// value, or the state would pass for a session.
	if c := jarCookie(client.Jar, d.hub); c != "" && c != stateCookie {
		return c, nil
	}
	return "", fmt.Errorf("%w (it answered %s and left the %s cookie as it was)", errNoSessionMinted, resp.Status, sessionCookieName)
}

func jarCookie(jar http.CookieJar, u *url.URL) string {
	for _, c := range jar.Cookies(u) {
		if c.Name == sessionCookieName {
			return c.Value
		}
	}
	return ""
}

// loginErrorReason is bounded because it is returned in an error message.
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
		// Not %w and not rawURL: the stdlib error quotes the URL, and the
		// callback request carries the authorization code in its query.
		return nil, fmt.Errorf("remoteproxy: login: building the request: %v", redactURL(err))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, requoted(req.URL.Path, err)
	}
	return resp, nil
}

// requoted restates a transport failure without its URL: a *url.Error quotes
// the full URL, and the callback's query carries the authorization code,
// which would reach the audit log and remote.log.
func requoted(path string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s %s: %w", ue.Op, path, ue.Err)
	}
	return err
}

// redactURL strips a query string from an error that is not a *url.Error.
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

// recordLogin never records the cookie; err is lever's own message.
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

// unnamedOperator is the placeholder hub identity when remote.allowed_users
// is unset: the gate then withholds the unverified identity header (see
// gate.operatorFor), so this is not a claim about who is connected.
const (
	unnamedOperator      = "lever-operator"
	unnamedOperatorEmail = "lever-operator@lever.local"
)

// IDEmailDomain is the email domain for a login that is not an address (such
// as exe.dev's X-ExeDev-UserID). config.validRemoteLogin keeps this domain out
// of allowed_users so no login can alias another's hub user; a test pins that
// the packages agree.
const IDEmailDomain = "id.lever.local"

// identityFor maps a verified login to hub claims. scion keys users on email,
// so each operator gets its own hub user. A login without "@" becomes
// <id>@IDEmailDomain, an undeliverable domain that cannot be mistaken for a
// real address; config.validRemoteLogin limits such ids to plain local-part
// characters, so the result cannot spell another address.
func identityFor(login string) Identity {
	if login == "" {
		return Identity{Subject: unnamedOperator, Email: unnamedOperatorEmail, Name: "Lever operator"}
	}
	return Identity{Subject: "lever-remote:" + login, Email: config.HubEmailFor(login), Name: login}
}

// HubUserEmails returns the hub user emails sign-ins can create: one per
// allowed login, or the placeholder operator when the list is empty.
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
