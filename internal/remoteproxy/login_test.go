package remoteproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubauth"
)

type auditSink struct {
	mu    sync.Mutex
	lines []AuditLine
}

func (s *auditSink) add(l AuditLine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, l)
}

func (s *auditSink) all() []AuditLine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AuditLine(nil), s.lines...)
}

func assertNoSecretsInAudit(t *testing.T, lines []AuditLine, secrets map[string]string) {
	t.Helper()
	if len(lines) == 0 {
		t.Fatal("no audit lines were emitted")
	}
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatalf("marshal audit line: %v", err)
		}
		for name, secret := range secrets {
			if secret == "" {
				t.Fatalf("test bug: empty %s", name)
			}
			if strings.Contains(string(b), secret) {
				t.Fatalf("audit line leaks the %s: %s", name, b)
			}
		}
	}
}

func randHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// assertionPrefix marks what testSigner signs; the fake hub accepts nothing
// else as an assertion.
const assertionPrefix = "signed."

// testSigner stands in for hubauth.RemoteSigner. Each assertion is unique,
// as a real one is (jti).
type testSigner struct {
	mu     sync.Mutex
	signed []string // emails, in order
	issued []string // assertions, in order
	fail   error
}

func (s *testSigner) sign(email string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return "", s.fail
	}
	a := assertionPrefix + email + "." + randHex()
	s.signed = append(s.signed, email)
	s.issued = append(s.issued, a)
	return a, nil
}

func (s *testSigner) all() (emails, assertions []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.signed...), append([]string(nil), s.issued...)
}

// hubUserIDFor is the id the fake hub gives an email, and the id the tests
// record unless they say otherwise.
func hubUserIDFor(email string) string { return "uid-" + email }

// fakeScionHub models the parts of scion's proxy auth mode the login and the
// proxy touch: GET / with an assertion creates a session cookie; a
// cookie-less request without one gets a fresh cookie that is not a session;
// /api/v1/auth/me answers the session's user id; the shell and the API need
// a session.
type fakeScionHub struct {
	*httptest.Server

	mu         sync.Mutex
	sessions   map[string]string // session cookie value -> email
	logins     int               // GET / requests carrying an assertion
	assertions []seenAssertion   // every request that carried the header
	meCalls    []http.Header     // headers of every /api/v1/auth/me request
	apiCalls   []http.Header     // headers of every other /api/v1 request
	elsewhere  int               // requests to /elsewhere (a followed redirect)

	loginStatus     int               // non-zero: answer the login with this status
	loginLocation   string            // with loginStatus 3xx: the Location
	noCookie        bool              // answer the login 200 without a cookie
	anonymousCookie bool              // answer the login with a cookie that is no session
	meOverride      map[string]string // email -> the id auth/me answers instead
	rejectSessions  bool              // treat every session as unknown (a restarted hub)
	beforeLogin     func()            // runs at the top of the login, to hold it open
}

type seenAssertion struct {
	method, path, value string
}

func newFakeScionHub(t *testing.T) *fakeScionHub {
	t.Helper()
	h := &fakeScionHub{sessions: map[string]string{}, meOverride: map[string]string{}}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.Server.Close)
	return h
}

func (h *fakeScionHub) serve(w http.ResponseWriter, r *http.Request) {
	if v := r.Header.Values(hubauth.Header); len(v) > 0 {
		h.mu.Lock()
		h.assertions = append(h.assertions, seenAssertion{r.Method, r.URL.Path, strings.Join(v, ",")})
		h.mu.Unlock()
		if r.URL.Path == "/" && r.Method == http.MethodGet {
			h.handleLogin(w, r, v[0])
			return
		}
	}
	switch {
	case r.URL.Path == "/elsewhere":
		h.mu.Lock()
		h.elsewhere++
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case r.URL.Path == authMePath:
		h.handleMe(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/"):
		h.handleAPI(w, r)
	default:
		h.handleShell(w, r)
	}
}

func (h *fakeScionHub) handleLogin(w http.ResponseWriter, r *http.Request, assertion string) {
	if h.beforeLogin != nil {
		h.beforeLogin()
	}
	h.mu.Lock()
	h.logins++
	h.mu.Unlock()
	if h.loginStatus != 0 {
		if h.loginLocation != "" {
			w.Header().Set("Location", h.loginLocation)
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: randHex(), Path: "/"})
		w.WriteHeader(h.loginStatus)
		return
	}
	email, ok := strings.CutPrefix(assertion, assertionPrefix)
	if !ok {
		http.Error(w, "invalid assertion", http.StatusUnauthorized)
		return
	}
	email = email[:strings.LastIndex(email, ".")]
	if h.noCookie {
		_, _ = fmt.Fprint(w, "<!doctype html>")
		return
	}
	sess := randHex()
	if !h.anonymousCookie {
		h.mu.Lock()
		h.sessions[sess] = email
		h.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: sess, Path: "/"})
	_, _ = fmt.Fprint(w, "<!doctype html>")
}

func (h *fakeScionHub) handleMe(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.meCalls = append(h.meCalls, r.Header.Clone())
	h.mu.Unlock()
	email, ok := h.sessionEmail(r)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	h.mu.Lock()
	id, over := h.meOverride[email]
	h.mu.Unlock()
	if !over {
		id = hubUserIDFor(email)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "email": email})
}

func (h *fakeScionHub) handleShell(w http.ResponseWriter, r *http.Request) {
	if email, ok := h.sessionEmail(r); ok {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "shell for %s", email)
		return
	}
	if r.URL.Path == "/" {
		http.Redirect(w, r, "/auth/login", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"authentication required"}`))
}

func (h *fakeScionHub) sessionEmail(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "", false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rejectSessions {
		return "", false
	}
	email, ok := h.sessions[c.Value]
	return email, ok
}

// handleAPI models sessionToBearerMiddleware: a request with an Authorization
// header passes through to the hub's own auth (where the narrow PAT is checked).
func (h *fakeScionHub) handleAPI(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.apiCalls = append(h.apiCalls, r.Header.Clone())
	h.mu.Unlock()
	if r.Header.Get("Authorization") != "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if email, ok := h.sessionEmail(r); ok {
		_, _ = fmt.Fprintf(w, `{"ok":true,"via":"session","user":%q}`, email)
		return
	}
	w.WriteHeader(http.StatusUnauthorized)
}

func (h *fakeScionHub) loginCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logins
}

func (h *fakeScionHub) seen() []seenAssertion {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]seenAssertion(nil), h.assertions...)
}

// recordedUsers is the HubUserID lookup: every email maps to the fake hub's
// id unless it is in missing or overridden.
type recordedUsers struct {
	mu       sync.Mutex
	missing  map[string]bool
	override map[string]string
}

func (u *recordedUsers) lookup(email string) (string, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.missing[email] {
		return "", false
	}
	if id, ok := u.override[email]; ok {
		return id, true
	}
	return hubUserIDFor(email), true
}

type testDriver struct {
	*LoginDriver
	hub    *fakeScionHub
	signer *testSigner
	users  *recordedUsers
	sink   *auditSink
}

func newTestDriver(t *testing.T) *testDriver {
	t.Helper()
	hub := newFakeScionHub(t)
	td := &testDriver{hub: hub, signer: &testSigner{}, users: &recordedUsers{missing: map[string]bool{}, override: map[string]string{}}, sink: &auditSink{}}
	td.LoginDriver = NewLoginDriver(LoginConfig{Hub: mustURL(t, hub.URL), Sign: td.signer.sign, HubUserID: td.users.lookup, Audit: td.sink.add})
	return td
}

func (td *testDriver) decisions() []Decision {
	var out []Decision
	for _, l := range td.sink.all() {
		out = append(out, l.Decision)
	}
	return out
}

// Under /auth/ and /api/v1/ scion checks the assertion but creates no
// session, so a login there would never yield a cookie.
func TestHubLoginPathIsASessionRoute(t *testing.T) {
	if !strings.HasPrefix(hubLoginPath, "/") {
		t.Fatalf("hubLoginPath %q is not an absolute path", hubLoginPath)
	}
	for _, public := range []string{"/auth/", "/api/v1/"} {
		if strings.HasPrefix(hubLoginPath, public) || hubLoginPath+"/" == public {
			t.Fatalf("hubLoginPath %q is under %s, where scion creates no session", hubLoginPath, public)
		}
	}
}

func TestLoginSendsTheAssertionOnlyOnTheLoginRequest(t *testing.T) {
	td := newTestDriver(t)
	cookie, err := td.Cookie(context.Background(), "op@example.test")
	if err != nil {
		t.Fatalf("Cookie: %v", err)
	}
	if cookie == "" {
		t.Fatal("login returned an empty session")
	}
	emails, issued := td.signer.all()
	if len(emails) != 1 || emails[0] != "op@example.test" {
		t.Fatalf("signed for %v, want exactly the login's hub email", emails)
	}
	seen := td.hub.seen()
	if len(seen) != 1 || seen[0] != (seenAssertion{http.MethodGet, hubLoginPath, issued[0]}) {
		t.Fatalf("the hub saw the assertion on %+v, want only GET %s with the signed value", seen, hubLoginPath)
	}
	td.hub.mu.Lock()
	me := td.hub.meCalls
	sessEmail := td.hub.sessions[cookie]
	td.hub.mu.Unlock()
	if sessEmail != "op@example.test" {
		t.Fatalf("the returned cookie is the hub session of %q", sessEmail)
	}
	if len(me) != 1 {
		t.Fatalf("%d auth/me calls, want 1", len(me))
	}
	if me[0].Get("Cookie") != sessionCookieName+"="+cookie || me[0].Get("Authorization") != "" || me[0].Get(hubauth.Header) != "" {
		t.Fatalf("auth/me carried Cookie %q Authorization %q assertion %q, want the new session alone",
			me[0].Get("Cookie"), me[0].Get("Authorization"), me[0].Get(hubauth.Header))
	}
	if d := td.decisions(); len(d) != 1 || d[0] != DecisionHubSession {
		t.Fatalf("audit = %v, want one %s line", d, DecisionHubSession)
	}
}

func TestLoginSignsTheLowercasedHubEmail(t *testing.T) {
	td := newTestDriver(t)
	if _, err := td.Cookie(context.Background(), "Op@Example.Test"); err != nil {
		t.Fatalf("Cookie: %v", err)
	}
	if emails, _ := td.signer.all(); len(emails) != 1 || emails[0] != "op@example.test" {
		t.Fatalf("signed for %v, want the lowercased email", emails)
	}
}

func TestLoginOfTheUnnamedOperatorUsesItsPlaceholderEmail(t *testing.T) {
	td := newTestDriver(t)
	if _, err := td.Cookie(context.Background(), ""); err != nil {
		t.Fatalf("Cookie: %v", err)
	}
	if emails, _ := td.signer.all(); len(emails) != 1 || emails[0] != unnamedOperatorEmail {
		t.Fatalf("signed for %v, want %s", emails, unnamedOperatorEmail)
	}
}

// A hub session for another user (a renamed or swapped row) must never be
// handed to this login, and other logins keep working.
func TestLoginRefusesASessionForAnotherHubUser(t *testing.T) {
	td := newTestDriver(t)
	td.hub.meOverride["alice@example.test"] = hubUserIDFor("mallory@example.test")
	ctx := context.Background()

	_, err := td.Cookie(ctx, "alice@example.test")
	if !errors.Is(err, errUserMismatch) {
		t.Fatalf("alice: error = %v, want errUserMismatch", err)
	}
	if _, err := td.Cookie(ctx, "bob@example.test"); err != nil {
		t.Fatalf("bob was refused with alice: %v", err)
	}
	if _, err := td.Cookie(ctx, "alice@example.test"); !errors.Is(err, errUserMismatch) {
		t.Fatalf("alice again: error = %v, want errUserMismatch", err)
	}
	if n := td.hub.loginCount(); n != 3 {
		t.Fatalf("hub saw %d logins, want 3: a refused login must not be cached", n)
	}
	var refused []AuditLine
	for _, l := range td.sink.all() {
		if l.Decision == DecisionHubSessionRefused {
			refused = append(refused, l)
		}
	}
	if len(refused) != 2 || refused[0].TSLogin != "alice@example.test" || refused[0].Path != authMePath {
		t.Fatalf("refusal audit = %+v, want two lines for alice at %s", refused, authMePath)
	}
}

// Without a recorded id there is nothing to check the session against, and
// scion's dev user is never one a login may ride, so the hub is not even
// asked: a first sign-in would create or activate a user.
func TestLoginRefusesALoginWithNoRecordedHubUser(t *testing.T) {
	td := newTestDriver(t)
	td.users.missing["alice@example.test"] = true
	td.users.override["carol@example.test"] = ""
	td.users.override["dave@example.test"] = config.ScionDevUserID
	ctx := context.Background()

	for _, login := range []string{"alice@example.test", "carol@example.test", "dave@example.test"} {
		if _, err := td.Cookie(ctx, login); !errors.Is(err, errNoRecordedUser) {
			t.Fatalf("%s: error = %v, want errNoRecordedUser", login, err)
		}
	}
	if n := td.hub.loginCount(); n != 0 {
		t.Fatalf("hub saw %d logins for unrecorded users, want 0", n)
	}
	if emails, _ := td.signer.all(); len(emails) != 0 {
		t.Fatalf("signed for %v, want nothing signed for an unrecorded login", emails)
	}
	if _, err := td.Cookie(ctx, "bob@example.test"); err != nil {
		t.Fatalf("bob was refused with alice: %v", err)
	}
	if d := td.decisions(); len(d) != 4 || d[0] != DecisionHubSessionRefused || d[1] != DecisionHubSessionRefused ||
		d[2] != DecisionHubSessionRefused || d[3] != DecisionHubSession {
		t.Fatalf("audit = %v, want three refusals then a session", d)
	}
}

func TestLoginFailsOnAnythingButA200(t *testing.T) {
	for _, tc := range []struct {
		status   int
		location string
	}{
		{http.StatusFound, "/elsewhere"},
		{http.StatusUnauthorized, ""},
		{http.StatusForbidden, ""},
		{http.StatusInternalServerError, ""},
		{http.StatusNoContent, ""},
	} {
		td := newTestDriver(t)
		td.hub.loginStatus, td.hub.loginLocation = tc.status, tc.location
		_, err := td.Cookie(context.Background(), "op@example.test")
		if !errors.Is(err, errLoginStatus) {
			t.Fatalf("status %d: error = %v, want errLoginStatus", tc.status, err)
		}
		_, issued := td.signer.all()
		if strings.Contains(err.Error(), issued[0]) {
			t.Fatalf("status %d: the error quotes the assertion: %v", tc.status, err)
		}
		td.hub.mu.Lock()
		elsewhere, me := td.hub.elsewhere, len(td.hub.meCalls)
		td.hub.mu.Unlock()
		if elsewhere != 0 {
			t.Fatalf("status %d: the login followed the hub's redirect, carrying the assertion", tc.status)
		}
		if me != 0 {
			t.Fatalf("status %d: auth/me was asked with a cookie from a failed login", tc.status)
		}
		if d := td.decisions(); len(d) != 1 || d[0] != DecisionHubSessionFailed {
			t.Fatalf("status %d: audit = %v, want one %s line", tc.status, d, DecisionHubSessionFailed)
		}
	}
}

func TestLoginFailsWithoutASessionCookie(t *testing.T) {
	td := newTestDriver(t)
	td.hub.noCookie = true
	if _, err := td.Cookie(context.Background(), "op@example.test"); !errors.Is(err, errNoSession) {
		t.Fatalf("error = %v, want errNoSession", err)
	}
}

// scion sets a fresh scion_sess on a request that has none, session or not:
// the cookie alone proves nothing until auth/me accepts it.
func TestACookieThatIsNoSessionFails(t *testing.T) {
	td := newTestDriver(t)
	td.hub.anonymousCookie = true
	_, err := td.Cookie(context.Background(), "op@example.test")
	if !errors.Is(err, errLoginStatus) || !strings.Contains(err.Error(), authMePath) {
		t.Fatalf("error = %v, want auth/me's refusal", err)
	}
}

func TestASigningFailureNeverReachesTheHub(t *testing.T) {
	td := newTestDriver(t)
	td.signer.fail = errors.New("hubauth: \"op@example.test\" is not an allowed remote login")
	if _, err := td.Cookie(context.Background(), "op@example.test"); err == nil {
		t.Fatal("logged in without an assertion")
	}
	if n := td.hub.loginCount(); n != 0 || len(td.hub.seen()) != 0 {
		t.Fatalf("hub saw %d logins after a signing failure", n)
	}
}

func TestConcurrentCallersShareOneLogin(t *testing.T) {
	td := newTestDriver(t)
	const callers = 12
	var wg sync.WaitGroup
	cookies := make([]string, callers)
	errs := make([]error, callers)
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			cookies[i], errs[i] = td.Cookie(context.Background(), "op@example.test")
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if cookies[i] != cookies[0] {
			t.Fatalf("caller %d got a different session", i)
		}
	}
	if n := td.hub.loginCount(); n != 1 {
		t.Fatalf("hub saw %d logins for one page load, want 1", n)
	}
}

func TestFailedLoginIsNotCached(t *testing.T) {
	td := newTestDriver(t)
	td.hub.loginStatus = http.StatusServiceUnavailable
	if _, err := td.Cookie(context.Background(), "op@example.test"); err == nil {
		t.Fatal("want an error")
	}
	td.hub.loginStatus = 0
	if _, err := td.Cookie(context.Background(), "op@example.test"); err != nil {
		t.Fatalf("second attempt after a transient failure: %v", err)
	}
}

// scion refuses a scion_sess older than 24 hours and a refused POST cannot be
// retried, so the driver renews at sessionMaxAge.
func TestSessionIsRenewedBeforeTheHubRefusesIt(t *testing.T) {
	td := newTestDriver(t)
	clock := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
	td.now = func() time.Time { return clock }
	ctx := context.Background()
	first, err := td.Cookie(ctx, "op@example.test")
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(sessionMaxAge - time.Minute)
	if c, _ := td.Cookie(ctx, "op@example.test"); c != first {
		t.Fatal("a session younger than sessionMaxAge was replaced")
	}
	clock = clock.Add(time.Minute)
	renewed, err := td.Cookie(ctx, "op@example.test")
	if err != nil || renewed == first {
		t.Fatalf("at sessionMaxAge: %q/%v, want a fresh session", renewed, err)
	}
	if n := td.hub.loginCount(); n != 2 {
		t.Fatalf("hub saw %d logins, want 2", n)
	}
	if _, issued := td.signer.all(); len(issued) != 2 || issued[0] == issued[1] {
		t.Fatalf("assertions %v, want a fresh one per login", issued)
	}
	if sessionMaxAge != 12*time.Hour {
		t.Fatalf("sessionMaxAge = %v, want 12h: under scion's 24h cookie MaxAge", sessionMaxAge)
	}
}

func TestInvalidateForcesAFreshLogin(t *testing.T) {
	td := newTestDriver(t)
	ctx := context.Background()
	first, err := td.Cookie(ctx, "op@example.test")
	if err != nil {
		t.Fatalf("Cookie: %v", err)
	}
	again, err := td.Cookie(ctx, "op@example.test")
	if err != nil || again != first {
		t.Fatalf("second call = %q/%v, want the cached session", again, err)
	}
	if n := td.hub.loginCount(); n != 1 {
		t.Fatalf("hub saw %d logins, want 1 (the session must be cached)", n)
	}

	// A stale cookie value must not discard a session someone else renewed.
	td.Invalidate("op@example.test", "some-older-cookie")
	if c, _ := td.Cookie(ctx, "op@example.test"); c != first {
		t.Fatal("invalidating a stale cookie value dropped the live session")
	}

	td.Invalidate("op@example.test", first)
	third, err := td.Cookie(ctx, "op@example.test")
	if err != nil {
		t.Fatalf("Cookie after invalidate: %v", err)
	}
	if third == first {
		t.Fatal("invalidate did not force a new session")
	}
	if n := td.hub.loginCount(); n != 2 {
		t.Fatalf("hub saw %d logins, want 2", n)
	}
}

// Two operators must become two hub users, not share one.
func TestDistinctOperatorsGetDistinctSessions(t *testing.T) {
	td := newTestDriver(t)
	ctx := context.Background()
	a, err := td.Cookie(ctx, "alice@example.test")
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	b, err := td.Cookie(ctx, "bob@example.test")
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	if a == b {
		t.Fatal("two operators shared one hub session")
	}
	td.hub.mu.Lock()
	defer td.hub.mu.Unlock()
	if td.hub.sessions[a] != "alice@example.test" || td.hub.sessions[b] != "bob@example.test" {
		t.Fatalf("hub recorded %q and %q", td.hub.sessions[a], td.hub.sessions[b])
	}
}

func TestIdentityForAnUnnamedOperator(t *testing.T) {
	id := identityFor("")
	if id.Email != unnamedOperatorEmail || id.Subject != unnamedOperator {
		t.Fatalf("identityFor(\"\") = %+v", id)
	}
	named := identityFor("op@example.test")
	if named.Email != "op@example.test" {
		t.Fatalf("identityFor(login).Email = %q, want the verified login", named.Email)
	}
}

// The emails the bootstrap step binds must be the ones sign-in creates.
func TestHubUserEmailsMatchIdentityFor(t *testing.T) {
	if got := HubUserEmails(nil); len(got) != 1 || got[0] != unnamedOperatorEmail {
		t.Fatalf("HubUserEmails(nil) = %v, want the placeholder", got)
	}
	got := HubUserEmails([]string{"a@github", "b@github"})
	if len(got) != 2 || got[0] != identityFor("a@github").Email || got[1] != identityFor("b@github").Email {
		t.Fatalf("HubUserEmails = %v", got)
	}
}

func TestLoginAuditNeverCarriesTheSessionOrTheAssertion(t *testing.T) {
	td := newTestDriver(t)
	cookie, err := td.Cookie(context.Background(), "op@example.test")
	if err != nil {
		t.Fatalf("Cookie: %v", err)
	}
	td.hub.anonymousCookie = true
	td.Invalidate("op@example.test", cookie)
	if _, err := td.Cookie(context.Background(), "op@example.test"); err == nil {
		t.Fatal("want a failure to audit")
	}
	td.hub.anonymousCookie = false
	td.hub.meOverride["op@example.test"] = "someone-else"
	if _, err := td.Cookie(context.Background(), "op@example.test"); err == nil {
		t.Fatal("want a refusal to audit")
	}
	_, issued := td.signer.all()
	secrets := map[string]string{"session cookie": cookie}
	for i, a := range issued {
		secrets[fmt.Sprintf("assertion %d", i)] = a
	}
	assertNoSecretsInAudit(t, td.sink.all(), secrets)
	if d := td.decisions(); len(d) != 3 || d[0] != DecisionHubSession || d[1] != DecisionHubSessionFailed || d[2] != DecisionHubSessionRefused {
		t.Fatalf("audit = %v", d)
	}
}

// A client that navigates away mid-login must not cancel the login others
// are waiting on.
func TestALoginSurvivesTheRequestThatStartedIt(t *testing.T) {
	td := newTestDriver(t)
	release := make(chan struct{})
	td.hub.beforeLogin = func() { <-release }

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		cookie string
		err    error
	}
	first := make(chan result, 1)
	go func() {
		c, err := td.Cookie(ctx, "op@example.test")
		first <- result{c, err}
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	close(release)

	if r := <-first; r.err != nil || r.cookie == "" {
		t.Fatalf("the login was abandoned with its request: %+v", r)
	}
	if c, err := td.Cookie(context.Background(), "op@example.test"); err != nil || c == "" {
		t.Fatalf("no usable session afterwards: %q %v", c, err)
	}
	if n := td.hub.loginCount(); n != 1 {
		t.Fatalf("hub saw %d logins, want 1", n)
	}
}

// A dropped jail transport mid-login must not put the assertion in the error
// or the audit log.
func TestATransportFailureNeverQuotesTheAssertion(t *testing.T) {
	td := newTestDriver(t)
	td.hub.beforeLogin = func() { panic(http.ErrAbortHandler) }
	_, err := td.Cookie(context.Background(), "op@example.test")
	if err == nil {
		t.Fatal("want the login to fail")
	}
	_, issued := td.signer.all()
	if len(issued) != 1 {
		t.Fatalf("%d assertions signed, want 1", len(issued))
	}
	if strings.Contains(err.Error(), issued[0]) {
		t.Fatalf("the returned error quotes the assertion: %v", err)
	}
	assertNoSecretsInAudit(t, td.sink.all(), map[string]string{"assertion": issued[0]})
}

// net/http recovers a handler panic, so the single-flight entry must still be
// released or every later request for that login waits forever. The panic is
// injected through the audit sink.
func TestAPanickingAttemptDoesNotWedgeTheDriver(t *testing.T) {
	hub := newFakeScionHub(t)
	var attempts atomic.Int32
	d := NewLoginDriver(LoginConfig{
		Hub:       mustURL(t, hub.URL),
		Sign:      (&testSigner{}).sign,
		HubUserID: (&recordedUsers{}).lookup,
		Audit: func(AuditLine) {
			if attempts.Add(1) == 1 {
				panic("audit sink exploded")
			}
		},
	})

	const operator = "op@example.test"
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("the panic was swallowed: net/http's own recovery, and the stack trace it logs, is what names the real fault")
			}
		}()
		_, _ = d.Cookie(context.Background(), operator)
	}()

	// Background context: if the panicked attempt is still in the map, this
	// never returns.
	type result struct {
		cookie string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		c, err := d.Cookie(context.Background(), operator)
		done <- result{c, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || r.cookie == "" {
			t.Fatalf("the login after a panicking one failed: %q %v", r.cookie, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second login for the same operator never returned: the panicked attempt left its single-flight entry unreleased")
	}
	if n := hub.loginCount(); n != 2 {
		t.Fatalf("hub saw %d logins, want 2 (the panicked attempt, then a real retry)", n)
	}
}

// Waiters on a panicked attempt must get an error: the zero cookie a panic
// leaves would otherwise read as an empty session.
func TestAPanickingAttemptReleasesTheWaitersOnIt(t *testing.T) {
	hub := newFakeScionHub(t)
	reached, release := make(chan struct{}), make(chan struct{})
	hub.beforeLogin = func() { close(reached); <-release }
	d := NewLoginDriver(LoginConfig{
		Hub:       mustURL(t, hub.URL),
		Sign:      (&testSigner{}).sign,
		HubUserID: (&recordedUsers{}).lookup,
		Audit:     func(AuditLine) { panic("audit sink exploded") },
	})

	const operator = "op@example.test"
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		_, _ = d.Cookie(context.Background(), operator)
	}()

	<-reached
	d.mu.Lock()
	e := d.sessions[operator]
	d.mu.Unlock()
	if e == nil {
		t.Fatal("test bug: no in-flight entry while the login is held open")
	}
	close(release)
	if r := <-panicked; r == nil {
		t.Fatal("test bug: the sink did not panic, so nothing here was exercised")
	}

	select {
	case <-e.done:
	default:
		t.Fatal("the attempt's channel is still open: every caller waiting on it stays parked for the life of the process")
	}
	if e.err == nil {
		t.Fatal("a waiter would read the panicked attempt as a successful login carrying an empty session")
	}
}
