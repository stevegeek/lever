package remoteproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubauth"
)

// The server-side login. The hub runs in scion's proxy auth mode and trusts
// a lever-signed assertion in hubauth.Header:
//
//  1. GET hubLoginPath with a fresh assertion for the login's hub email; the
//     hub answers with a scion_sess cookie that is now a session.
//  2. GET authMePath with that cookie alone; the user id must be the one
//     `lever apply` recorded for that email.
//
// The assertion is sent on step 1 only, and never on a forwarded request
// (rewriteUpstream strips any client copy). The session cookie stays
// host-side: proxy.go injects it hub-bound and strips it from every response,
// so the browser never holds a hub credential.

const (
	sessionCookieName = "scion_sess"

	// hubLoginPath must be a route scion's web layer runs its session
	// middleware on: under /auth/ and /api/v1/ the assertion is checked but
	// no session is created.
	hubLoginPath = "/"
	authMePath   = "/api/v1/auth/me"

	loginTimeout = 30 * time.Second

	// scion refuses a scion_sess older than 24 hours as if there were no
	// session, and a spent POST body cannot be retried, so renew well before.
	// The hub refreshes the 15-minute access token inside the cookie itself.
	sessionMaxAge = 12 * time.Hour

	// maxLoginBody only drains for keep-alive: the login reads no body.
	maxLoginBody = 8 << 10
	// maxAuthMeBody bounds the one body the login decodes.
	maxAuthMeBody = 64 << 10
)

type LoginConfig struct {
	// Hub is the hub base URL as dialled (as Config.Target).
	Hub *url.URL
	// DialContext reaches Hub (the jail dial); nil uses the default dialer.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// Sign returns a hub assertion for a lowercased hub email. Required.
	Sign func(email string) (string, error)
	// HubUserID returns the hub user id recorded for a lowercased hub email.
	// A login without one is refused. Required.
	HubUserID func(email string) (string, bool)
	// Audit receives one line per login outcome; nil disables. It never
	// carries the assertion or the cookie.
	Audit func(line AuditLine)
}

// LoginDriver obtains and caches hub sessions, one per verified operator.
type LoginDriver struct {
	hub       *url.URL
	client    *http.Client
	sign      func(string) (string, error)
	hubUserID func(string) (string, bool)
	audit     func(AuditLine)

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

// NewLoginDriver builds the driver. Its client has no jar and never follows a
// redirect: a redirect would carry the assertion to wherever the hub points.
func NewLoginDriver(cfg LoginConfig) *LoginDriver {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.DialContext != nil {
		t.DialContext = cfg.DialContext
	}
	// An operator's HTTP_PROXY must never route the login off the host.
	t.Proxy = nil
	return &LoginDriver{
		hub:       cfg.Hub,
		client:    &http.Client{Transport: t, CheckRedirect: noRedirect},
		sign:      cfg.Sign,
		hubUserID: cfg.HubUserID,
		audit:     cfg.Audit,
		now:       time.Now,
		sessions:  map[string]*sessionEntry{},
	}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (d *LoginDriver) hubURL(path string) string {
	return strings.TrimSuffix(d.hub.String(), "/") + path
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
// sessionMaxAge; a running attempt is never expired. Called with d.mu held.
func (d *LoginDriver) expired(e *sessionEntry) bool {
	select {
	case <-e.done:
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
	errNoRecordedUser = errors.New("remoteproxy: login: no hub user is recorded for this login; run `lever apply`")
	errUserMismatch   = errors.New("remoteproxy: login: the hub session belongs to another hub user than the one recorded for this login")
	errNoSession      = errors.New("remoteproxy: login: the hub set no session cookie")
	errLoginStatus    = errors.New("remoteproxy: login: the hub refused the login")
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

	email := strings.ToLower(identityFor(operator).Email)
	// Checked before the hub is asked, so an unrecorded login never reaches
	// the hub, where a first sign-in would create or activate a user.
	want, ok := d.lookupUser(email)
	if !ok {
		d.recordRefused(operator, hubLoginPath, errNoRecordedUser)
		return "", errNoRecordedUser
	}
	if d.sign == nil {
		err := errors.New("remoteproxy: login: no assertion signer")
		d.recordLogin(operator, 0, err)
		return "", err
	}
	assertion, err := d.sign(email)
	if err != nil {
		// The signer's error is lever's own text, but bound it anyway.
		err = fmt.Errorf("remoteproxy: login: sign the assertion: %s", truncateAudit(err.Error()))
		d.recordLogin(operator, 0, err)
		return "", err
	}
	cookie, err := d.session(ctx, assertion)
	if err != nil {
		d.recordLogin(operator, 0, err)
		return "", err
	}
	got, err := d.whoAmI(ctx, cookie)
	if err != nil {
		d.recordLogin(operator, 0, err)
		return "", err
	}
	if got != want {
		d.recordRefused(operator, authMePath, errUserMismatch)
		return "", errUserMismatch
	}
	d.recordLogin(operator, http.StatusOK, nil)
	return cookie, nil
}

func (d *LoginDriver) lookupUser(email string) (string, bool) {
	if d.hubUserID == nil {
		return "", false
	}
	id, ok := d.hubUserID(email)
	// scion's dev user is lever's controller on a migrated hub and owns every
	// agent; no remote login may ride it, whatever a record says.
	return id, ok && id != "" && id != config.ScionDevUserID
}

// session sends the assertion and takes the session cookie. Neither the
// assertion nor any cookie value appears in an error.
func (d *LoginDriver) session(ctx context.Context, assertion string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.hubURL(hubLoginPath), nil)
	if err != nil {
		return "", fmt.Errorf("remoteproxy: login: building the request: %w", err)
	}
	req.Header.Set(hubauth.Header, assertion)
	resp, err := d.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("remoteproxy: login: %w", requoted(hubLoginPath, err))
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: it answered %d to GET %s", errLoginStatus, resp.StatusCode, hubLoginPath)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			return c.Value, nil
		}
	}
	return "", fmt.Errorf("%w (GET %s answered %d)", errNoSession, hubLoginPath, resp.StatusCode)
}

// whoAmI reads the session's hub user id. A cookie the hub minted without a
// session fails here rather than passing for one.
func (d *LoginDriver) whoAmI(ctx context.Context, cookie string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.hubURL(authMePath), nil)
	if err != nil {
		return "", fmt.Errorf("remoteproxy: login: building the request: %w", err)
	}
	req.Header.Set("Cookie", sessionCookieName+"="+cookie)
	req.Header.Set("Accept", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("remoteproxy: login: %w", requoted(authMePath, err))
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: it answered %d to GET %s with the new session", errLoginStatus, resp.StatusCode, authMePath)
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAuthMeBody)).Decode(&me); err != nil {
		return "", fmt.Errorf("remoteproxy: login: GET %s: the answer is not the expected JSON", authMePath)
	}
	if me.ID == "" {
		return "", fmt.Errorf("remoteproxy: login: GET %s named no user id", authMePath)
	}
	return me.ID, nil
}

// requoted restates a transport failure without its URL.
func requoted(path string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s %s: %w", ue.Op, path, ue.Err)
	}
	return err
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxLoginBody))
	_ = resp.Body.Close()
}

// recordLogin never records the assertion or the cookie; err is lever's own
// message.
func (d *LoginDriver) recordLogin(operator string, status int, err error) {
	if d.audit == nil {
		return
	}
	line := AuditLine{
		Time:     time.Now().UTC(),
		TSLogin:  truncateAudit(operator),
		Method:   http.MethodGet,
		Path:     hubLoginPath,
		Decision: DecisionHubSession,
		Status:   status,
	}
	if err != nil {
		line.Decision, line.Error = DecisionHubSessionFailed, err.Error()
	}
	d.audit(line)
}

func (d *LoginDriver) recordRefused(operator, path string, err error) {
	if d.audit == nil {
		return
	}
	d.audit(AuditLine{
		Time:     time.Now().UTC(),
		TSLogin:  truncateAudit(operator),
		Method:   http.MethodGet,
		Path:     path,
		Decision: DecisionHubSessionRefused,
		Error:    err.Error(),
	})
}

// Identity is the hub identity a verified login maps to.
type Identity struct {
	Subject string
	Email   string // the hub keys its user row on this
	Name    string
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
