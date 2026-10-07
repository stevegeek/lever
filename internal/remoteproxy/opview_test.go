package remoteproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	viewOpCookie = "sess-op"
	viewCCookie  = "sess-c"
)

// loginSession hands out one cookie per login and records each login it is
// asked for: the operator view must never ask for an unbound contact's.
type loginSession struct {
	mu          sync.Mutex
	cookies     map[string]string
	renew       map[string]string // the cookie handed out after the key is invalidated
	asked       []string
	invalidated []string
}

func newLoginSession() *loginSession {
	return &loginSession{cookies: map[string]string{"op@x": viewOpCookie, "c@x": viewCCookie, "a/b@x": viewCCookie}, renew: map[string]string{}}
}

func (s *loginSession) Cookie(_ context.Context, login string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, login)
	c, ok := s.cookies[login]
	if !ok {
		return "", errors.New("no session for " + login)
	}
	return c, nil
}

func (s *loginSession) Invalidate(login, cookie string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated = append(s.invalidated, cookie)
	if n, ok := s.renew[cookie]; ok {
		s.cookies[login] = n
	}
}

func (s *loginSession) askedFor(login string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.asked {
		if l == login {
			n++
		}
	}
	return n
}

// viewHub answers the contact's DM history with historyBody, to the
// contact's session only, and records "METHOD URI COOKIE" per request.
// /api/v1/auth/me (the contact fence's question, and the operator view's
// after a failed read) is answered by me (the contact's id when nil) and
// recorded in meSeen, not seen.
type viewHub struct {
	*httptest.Server
	mu     sync.Mutex
	seen   []string
	meSeen []string
	answer func(w http.ResponseWriter, r *http.Request) bool
	me     func(w http.ResponseWriter, r *http.Request)
}

const viewDMPath = "/api/v1/chat/conversations/dm:agent:" + agentW1 + ":user:" + contactUID + "/messages"

func newViewHub(t *testing.T) *viewHub {
	h := &viewHub{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie := ""
		if c, err := r.Cookie(sessionCookieName); err == nil {
			cookie = c.Value
		}
		if r.URL.Path == "/api/v1/auth/me" {
			h.mu.Lock()
			h.meSeen = append(h.meSeen, r.Method+" "+cookie)
			me := h.me
			h.mu.Unlock()
			if me != nil {
				me(w, r)
				return
			}
			_, _ = io.WriteString(w, `{"id":"`+contactUID+`"}`)
			return
		}
		h.mu.Lock()
		h.seen = append(h.seen, r.Method+" "+r.URL.RequestURI()+" "+cookie)
		answer := h.answer
		h.mu.Unlock()
		if answer != nil && answer(w, r) {
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == viewDMPath && cookie == viewCCookie {
			_, _ = io.WriteString(w, historyBody)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *viewHub) reached() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.seen)
}

type viewAudit struct {
	mu    sync.Mutex
	lines []AuditLine
}

func (a *viewAudit) add(l AuditLine) { a.mu.Lock(); a.lines = append(a.lines, l); a.mu.Unlock() }

func (a *viewAudit) last() AuditLine {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lines[len(a.lines)-1]
}

func (a *viewAudit) all() []AuditLine { a.mu.Lock(); defer a.mu.Unlock(); return slices.Clone(a.lines) }

// viewConfig: manager boss; contact c@x messages w1 and boss and sees w2;
// contact d@x messages w1 and was never bound to a hub user.
func viewConfig(t *testing.T, hub *viewHub, sess *loginSession, audit *viewAudit) Config {
	t.Helper()
	return Config{Target: mustURL(t, hub.URL), Session: sess, ServeHost: testServeHost,
		AllowedUsers: []string{"op@x", "c@x", "d@x"},
		Contacts:     map[string][]string{"c@x": {"w1", "boss"}, "d@x": {"w1"}},
		ContactSee:   map[string][]string{"c@x": {"w2"}},
		ResolveAgents: func(context.Context) (map[string]string, error) {
			return map[string]string{"w1": agentW1, "w2": agentW2, "boss": agentMgr}, nil
		},
		ContactSession: func(string) error { return nil },
		ChatAgent:      "boss",
		Workers:        []string{"w1", "w2"},
		AgentRecords: func(context.Context) (map[string]AgentRecord, error) {
			return map[string]AgentRecord{"boss": {ID: agentMgr, Phase: "running"},
				"w1": {ID: agentW1, Phase: "running", Activity: "working"}, "w2": {ID: agentW2, Phase: "suspended"}}, nil
		},
		Labels: func() map[string]string { return map[string]string{"w1": "Via Roma"} },
		ContactUser: func(login string) (string, bool) {
			if login == "c@x" || login == "a/b@x" {
				return contactUID, true
			}
			return "", false
		},
		Audit: audit.add,
	}
}

func viewHandler(t *testing.T, cfg Config) http.Handler { t.Helper(); return NewHandler(cfg) }

func viewDo(h http.Handler, login, method, target string) *httptest.ResponseRecorder {
	return contactDo(h, login, method, target, "")
}

func TestOperatorViewListsContacts(t *testing.T) {
	hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
	h := viewHandler(t, viewConfig(t, hub, sess, audit))
	rw := viewDo(h, "op@x", "GET", "/lever/api/contacts")
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rw.Code, rw.Body)
	}
	want := `{"contacts":[{"login":"c@x","signedIn":true,"agents":[{"name":"boss","label":"","state":"running"},` +
		`{"name":"w1","label":"Via Roma","state":"running"}]},{"login":"d@x","signedIn":false,"agents":[{"name":"w1","label":"Via Roma","state":"running"}]}]}`
	if got := strings.TrimSpace(rw.Body.String()); got != want {
		t.Fatalf("list\n got %s\nwant %s", got, want)
	}
	for _, k := range []string{"Content-Type", "Cache-Control", "Content-Security-Policy"} {
		if want := map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store", "Content-Security-Policy": "sandbox"}[k]; rw.Header().Get(k) != want {
			t.Errorf("%s = %q, want %q", k, rw.Header().Get(k), want)
		}
	}
	if sess.askedFor("c@x")+sess.askedFor("d@x") != 0 {
		t.Fatal("listing contacts must not ask for any contact's session")
	}
	if len(hub.reached()) != 0 {
		t.Fatalf("the list reads no hub route: %v", hub.reached())
	}
	l := audit.last()
	if l.Decision != DecisionOperatorView || l.Count == nil || *l.Count != 2 || l.TSLogin != "op@x" {
		t.Fatalf("audit %+v", l)
	}
}

func TestOperatorViewRefusesContacts(t *testing.T) {
	hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
	h := viewHandler(t, viewConfig(t, hub, sess, audit))
	for _, method := range []string{"GET", "HEAD", "POST", "DELETE"} {
		for _, p := range []string{"/lever/api/contacts", "/lever/api/contacts/c%40x/agents/w1/messages", "/lever/api/contacts/x"} {
			rw := viewDo(h, "c@x", method, p)
			if rw.Code != http.StatusForbidden || audit.last().Decision != DecisionDenyContact {
				t.Fatalf("%s %s as a contact: %d %s (%s)", method, p, rw.Code, rw.Body, audit.last().Decision)
			}
		}
	}
	if len(hub.reached()) != 0 {
		t.Fatalf("a refused request reached the hub: %v", hub.reached())
	}
}

func TestOperatorViewOnlyReads(t *testing.T) {
	hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
	h := viewHandler(t, viewConfig(t, hub, sess, audit))
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, p := range []string{"/lever/api/contacts", "/lever/api/contacts/c%40x/agents/w1/messages"} {
			rw := viewDo(h, "op@x", method, p)
			if rw.Code != http.StatusMethodNotAllowed || rw.Header().Get("Allow") != "GET, HEAD" {
				t.Fatalf("%s %s: %d Allow=%q", method, p, rw.Code, rw.Header().Get("Allow"))
			}
		}
	}
	if sess.askedFor("c@x") != 0 || len(hub.reached()) != 0 {
		t.Fatal("a write must not touch the contact's session or the hub")
	}
}

func TestOperatorViewNeedsTheChatPage(t *testing.T) {
	hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
	cfg := viewConfig(t, hub, sess, audit)
	cfg.ChatAgent = ""
	viewDo(viewHandler(t, cfg), "op@x", "GET", "/lever/api/contacts")
	if sess.askedFor("c@x") != 0 {
		t.Fatal("with the chat page off there is no operator view")
	}
	if got := hub.reached(); len(got) != 1 || got[0] != "GET /lever/api/contacts "+viewOpCookie {
		t.Fatalf("with the page off the path is the hub's, as every /lever path is: %v", got)
	}
}

func viewMessages(t *testing.T, rw *httptest.ResponseRecorder) viewAnswer {
	t.Helper()
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rw.Code, rw.Body)
	}
	var a viewAnswer
	if err := json.Unmarshal(rw.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

const viewC = "/lever/api/contacts/c%40x/agents/w1/messages"

func TestOperatorViewReadsWithTheContactSession(t *testing.T) {
	hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
	h := viewHandler(t, viewConfig(t, hub, sess, audit))
	viewMessages(t, viewDo(h, "op@x", "GET", viewC))
	want := []string{"GET " + viewDMPath + "?limit=50 " + viewCCookie}
	if got := hub.reached(); !slices.Equal(got, want) {
		t.Fatalf("hub saw %v, want exactly %v", got, want)
	}
}

func TestOperatorViewRowMapping(t *testing.T) {
	hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
	rows := func(a viewAnswer) string {
		var b strings.Builder
		for _, m := range a.Messages {
			fmt.Fprintf(&b, "%s %s %q %v|", m.ID, m.From, m.Text, m.ShownToContact)
			if m.Pending {
				b.WriteString("pending|")
			}
		}
		return b.String()
	}
	// Agent messages on: only the recorded (and bound) agent row is shown
	// to the contact.
	cfg := viewConfig(t, hub, sess, audit)
	cfg.MatchAgentMessages, cfg.PeekAgentMessages = refuseMatch(t), peekOf(recordedOnly("recorded"), false)
	a := viewMessages(t, viewDo(viewHandler(t, cfg), "op@x", "GET", viewC))
	want := `a1 agent "recorded" true|a2 agent "SECRET unrecorded" false|a3 agent "SECRET as state" false|` +
		`c1 contact "mine" true|s1 system "agent started" true|`
	if got := rows(a); got != want || a.Contact != "c@x" || a.Agent != "w1" || a.NextCursor != "cur-1" || !a.Matched {
		t.Fatalf("on:\n got %s %+v\nwant %s", got, a, want)
	}
	if a.Messages[0].CreatedAt != "2026-10-06T10:00:00Z" {
		t.Fatalf("createdAt %q", a.Messages[0].CreatedAt)
	}
	// The same row, kept only because a record would bind it: not shown to
	// the contact yet, pending.
	cfg.PeekAgentMessages = peekOf(recordedOnly("recorded"), true)
	a = viewMessages(t, viewDo(viewHandler(t, cfg), "op@x", "GET", viewC))
	if got := rows(a); !strings.HasPrefix(got, `a1 agent "recorded" false|pending|a2 agent "SECRET unrecorded" false|`) || strings.Count(got, "pending") != 1 {
		t.Fatalf("pending: %s", got)
	}
	// A pending id the broker names for a row the rule does not keep (not
	// asked, or not an agent row) is no mark.
	cfg.PeekAgentMessages = func(context.Context, string, string, []AgentMessage) (map[string]bool, map[string]bool, error) {
		return map[string]bool{}, map[string]bool{"c1": true, "s1": true, "a2": true}, nil
	}
	a = viewMessages(t, viewDo(viewHandler(t, cfg), "op@x", "GET", viewC))
	if got := rows(a); strings.Contains(got, "pending") {
		t.Fatalf("pending outside keep: %s", got)
	}
	// Off: the contact sees the hub's answer as it is.
	cfg.MatchAgentMessages, cfg.PeekAgentMessages = nil, nil
	a = viewMessages(t, viewDo(viewHandler(t, cfg), "op@x", "GET", viewC))
	if got := rows(a); strings.Contains(got, "false") {
		t.Fatalf("off: every row is shown to the contact: %s", got)
	}
}

func TestOperatorViewMatcherFailure(t *testing.T) {
	hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
	cfg := viewConfig(t, hub, sess, audit)
	down := func(context.Context, string, string, []AgentMessage) (map[string]bool, map[string]bool, error) {
		return nil, map[string]bool{"a1": true}, errors.New("broker down")
	}
	// The broker down, and a peek that is not wired (the view never falls
	// back to the binding question).
	for _, peek := range []func(context.Context, string, string, []AgentMessage) (map[string]bool, map[string]bool, error){down, nil} {
		cfg.MatchAgentMessages, cfg.PeekAgentMessages = refuseMatch(t), peek
		a := viewMessages(t, viewDo(viewHandler(t, cfg), "op@x", "GET", viewC))
		if a.Matched || len(a.Messages) != 5 {
			t.Fatalf("matched=%v rows=%d: every row, and matched false", a.Matched, len(a.Messages))
		}
		for _, m := range a.Messages {
			if m.From == "agent" && (m.ShownToContact || m.Pending) {
				t.Fatalf("%s: with no answer the contact is shown no agent row", m.ID)
			}
		}
	}
}

// peekOf makes a matcher a peek: every kept row pending, or none (bound).
func peekOf(match func(context.Context, string, string, []AgentMessage) (map[string]bool, error), pending bool) func(context.Context, string, string, []AgentMessage) (map[string]bool, map[string]bool, error) {
	return func(ctx context.Context, contact, agent string, msgs []AgentMessage) (map[string]bool, map[string]bool, error) {
		keep, err := match(ctx, contact, agent, msgs)
		p := map[string]bool{}
		if pending {
			p = keep
		}
		return keep, p, err
	}
}

// refuseMatch is a binding matcher the operator view must never call.
func refuseMatch(t *testing.T) func(context.Context, string, string, []AgentMessage) (map[string]bool, error) {
	return func(context.Context, string, string, []AgentMessage) (map[string]bool, error) {
		t.Error("the operator view asked the binding question")
		return nil, errors.New("not for the operator view")
	}
}

// fakeLedger binds like the broker's agent ledger: one record for the
// text "same", bound to the earliest unbound row it is asked about; a peek
// answers the same and binds nothing.
type fakeLedger struct {
	mu    sync.Mutex
	bound string
}

func (l *fakeLedger) ask(bind bool) func(context.Context, string, string, []AgentMessage) (map[string]bool, error) {
	return func(_ context.Context, _, _ string, msgs []AgentMessage) (map[string]bool, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		keep := map[string]bool{}
		if l.bound != "" {
			for _, m := range msgs {
				keep[m.ID] = m.ID == l.bound
			}
			return keep, nil
		}
		msgs = slices.Clone(msgs)
		slices.SortFunc(msgs, func(a, b AgentMessage) int { return a.CreatedAt.Compare(b.CreatedAt) })
		for _, m := range msgs {
			if m.SHA256 == hashHex("same") {
				keep[m.ID] = true
				if bind {
					l.bound = m.ID
				}
				break
			}
		}
		return keep, nil
	}
}

// peek is ask without binding, with every kept unbound row pending.
func (l *fakeLedger) peek() func(context.Context, string, string, []AgentMessage) (map[string]bool, map[string]bool, error) {
	ask := l.ask(false)
	return func(ctx context.Context, contact, agent string, msgs []AgentMessage) (map[string]bool, map[string]bool, error) {
		l.mu.Lock()
		bound := l.bound
		l.mu.Unlock()
		keep, err := ask(ctx, contact, agent, msgs)
		pending := map[string]bool{}
		for id, k := range keep {
			if k && id != bound {
				pending[id] = true
			}
		}
		return keep, pending, err
	}
}

// TestOperatorViewPeeksAndBindsNothing: two agent rows of the same text and
// one record. The operator reads a page holding only the later one (it
// would be shown, were it read alone), and binds nothing: the contact's own
// read of both rows then shows the earliest, as with no operator read.
func TestOperatorViewPeeksAndBindsNothing(t *testing.T) {
	hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
	const m1 = `{"id":"m1","sender":"agent:w1","senderId":"id-w1","type":"instruction","msg":"same","createdAt":"2026-10-06T10:00:00Z"}`
	const m2 = `{"id":"m2","sender":"agent:w1","senderId":"id-w1","type":"instruction","msg":"same","createdAt":"2026-10-06T10:01:00Z"}`
	hub.answer = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != viewDMPath {
			return false
		}
		if r.URL.Query().Get("limit") == "1" {
			_, _ = io.WriteString(w, `{"messages":[`+m2+`]}`)
		} else {
			_, _ = io.WriteString(w, `{"messages":[`+m2+`,`+m1+`]}`)
		}
		return true
	}
	led := &fakeLedger{}
	cfg := viewConfig(t, hub, sess, audit)
	cfg.MatchAgentMessages, cfg.PeekAgentMessages = led.ask(true), led.peek()
	h := viewHandler(t, cfg)
	a := viewMessages(t, viewDo(h, "op@x", "GET", viewC+"?limit=1"))
	if len(a.Messages) != 1 || a.Messages[0].ShownToContact || !a.Messages[0].Pending || led.bound != "" {
		t.Fatalf("operator read: %+v bound=%q, want m2 pending (not yet read by the contact) and nothing bound", a.Messages, led.bound)
	}
	rw := contactDo(h, "c@x", "GET", dmPath(agentW1, contactUID, "/messages?limit=50"), "")
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), `"m1"`) || strings.Contains(rw.Body.String(), `"m2"`) || led.bound != "m1" {
		t.Fatalf("the contact's read after the operator's: %d %s bound=%q", rw.Code, rw.Body, led.bound)
	}
	// The operator's next read shows what the contact now sees.
	a = viewMessages(t, viewDo(h, "op@x", "GET", viewC))
	if len(a.Messages) != 2 || a.Messages[0].ID != "m2" || a.Messages[0].ShownToContact || a.Messages[0].Pending ||
		!a.Messages[1].ShownToContact || a.Messages[1].Pending {
		t.Fatalf("operator read after the binding: %+v", a.Messages)
	}
}

func TestOperatorViewAllowList(t *testing.T) {
	hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
	h := viewHandler(t, viewConfig(t, hub, sess, audit))
	for _, p := range []string{
		"/lever/api/contacts/x%40x/agents/w1/messages",       // not a login
		"/lever/api/contacts/op%40x/agents/w1/messages",      // the operator is not a contact
		"/lever/api/contacts/c%40x/agents/w2/messages",       // see-only agent
		"/lever/api/contacts/c%40x/agents/w3/messages",       // not one of its agents
		"/lever/api/contacts/c%40x/agents/W1/messages",       // not an agent name
		"/lever/api/contacts/c%40x/agents/w1",                // no messages part
		"/lever/api/contacts/c%40x/agents/w1/messages/extra", // more parts
		"/lever/api/contacts/c%40x/agent/w1/messages",        // wrong word
		"/lever/api/contacts/C%40x/agents/w1/messages",       // case differs
		"/lever/api/contacts/",
	} {
		rw := viewDo(h, "op@x", "GET", p)
		if rw.Code != http.StatusNotFound || strings.TrimSpace(rw.Body.String()) != `{"error":"not-found"}` {
			t.Errorf("%s: %d %s", p, rw.Code, rw.Body)
		}
	}
	// An encoded dot or slash never reaches the view: the proxy's path
	// check (pathcheck.go) refuses it first, for every route.
	for _, p := range []string{
		"/lever/api/contacts/%2E%2E/agents/w1/messages",      // dots
		"/lever/api/contacts/c%40x%2Fagents%2Fw1%2Fmessages", // one escaped segment
	} {
		if rw := viewDo(h, "op@x", "GET", p); rw.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", p, rw.Code, rw.Body)
		}
	}
	if sess.askedFor("c@x") != 0 || len(hub.reached()) != 0 {
		t.Fatalf("a refused path asked for a session (%d) or reached the hub (%v)", sess.askedFor("c@x"), hub.reached())
	}
}

func TestOperatorViewLoginEncoding(t *testing.T) {
	hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
	cfg := viewConfig(t, hub, sess, audit)
	cfg.AllowedUsers = append(cfg.AllowedUsers, "a/b@x")
	cfg.Contacts["a/b@x"] = []string{"w1"}
	h := viewHandler(t, cfg)
	// The proxy's path check refuses an encoded slash on every route, so a
	// login holding "/" cannot be opened in the view (the guide says so);
	// nothing is asked for it.
	if rw := viewDo(h, "op@x", "GET", "/lever/api/contacts/"+url.PathEscape("a/b@x")+"/agents/w1/messages"); rw.Code != http.StatusBadRequest {
		t.Fatalf("an escaped slash = %d %s", rw.Code, rw.Body)
	}
	if sess.askedFor("a/b@x") != 0 {
		t.Fatal("a refused path asked for a session")
	}
	if rw := viewDo(h, "op@x", "GET", "/lever/api/contacts/a/b@x/agents/w1/messages"); rw.Code != http.StatusNotFound {
		t.Fatalf("a raw slash splits the path: %d", rw.Code)
	}
}

func TestOperatorViewNeverSignedIn(t *testing.T) {
	hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
	h := viewHandler(t, viewConfig(t, hub, sess, audit))
	rw := viewDo(h, "op@x", "GET", "/lever/api/contacts/d%40x/agents/w1/messages")
	if rw.Code != http.StatusConflict || strings.TrimSpace(rw.Body.String()) != `{"error":"not-signed-in"}` {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	if sess.askedFor("d@x") != 0 || len(hub.reached()) != 0 {
		t.Fatal("an unbound contact must not get a session: the login would create its hub user")
	}
	if l := audit.last(); l.Decision != DecisionDenyOperatorView || l.Reason != "not-signed-in" || l.Contact != "d@x" || l.Agent != "w1" {
		t.Fatalf("audit %+v", l)
	}
}

func TestOperatorViewNoAgentRecord(t *testing.T) {
	hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
	cfg := viewConfig(t, hub, sess, audit)
	cfg.AgentRecords = func(context.Context) (map[string]AgentRecord, error) { return map[string]AgentRecord{}, nil }
	rw := viewDo(viewHandler(t, cfg), "op@x", "GET", viewC)
	if rw.Code != http.StatusConflict || strings.TrimSpace(rw.Body.String()) != `{"error":"no-record"}` || sess.askedFor("c@x") != 0 {
		t.Fatalf("%d %s asked=%d", rw.Code, rw.Body, sess.askedFor("c@x"))
	}
}

func TestOperatorViewQuery(t *testing.T) {
	hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
	h := viewHandler(t, viewConfig(t, hub, sess, audit))
	viewMessages(t, viewDo(h, "op@x", "GET", viewC+"?cursor=cur-1&limit=200&around=x&sub=y"))
	want := "GET " + viewDMPath + "?cursor=cur-1&limit=200 " + viewCCookie
	if got := hub.reached(); len(got) != 1 || got[0] != want {
		t.Fatalf("hub saw %v, want %s (only cursor and limit pass)", got, want)
	}
	for _, q := range []string{"limit=0", "limit=201", "limit=x", "cursor=a%20b", "cursor=" + strings.Repeat("c", 513), "cursor=%C3%A9"} {
		rw := viewDo(h, "op@x", "GET", viewC+"?"+q)
		if rw.Code != http.StatusBadRequest || strings.TrimSpace(rw.Body.String()) != `{"error":"bad-query"}` {
			t.Errorf("%s: %d %s", q, rw.Code, rw.Body)
		}
	}
}

func TestOperatorViewRetriesALapsedSession(t *testing.T) {
	for _, lapsed := range []int{http.StatusUnauthorized, http.StatusFound, http.StatusSeeOther} {
		hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
		sess.renew[viewCCookie] = "sess-c-2"
		hub.answer = func(w http.ResponseWriter, r *http.Request) bool {
			if c, _ := r.Cookie(sessionCookieName); c != nil && c.Value == viewCCookie {
				if lapsed != http.StatusUnauthorized {
					w.Header().Set("Location", "/login")
				}
				w.WriteHeader(lapsed)
				return true
			}
			if c, _ := r.Cookie(sessionCookieName); c != nil && c.Value == "sess-c-2" {
				_, _ = io.WriteString(w, historyBody)
				return true
			}
			return false
		}
		a := viewMessages(t, viewDo(viewHandler(t, viewConfig(t, hub, sess, audit)), "op@x", "GET", viewC))
		if len(a.Messages) != 5 || !slices.Equal(sess.invalidated, []string{viewCCookie}) || len(hub.reached()) != 2 {
			t.Fatalf("%d: rows=%d invalidated=%v hub=%v", lapsed, len(a.Messages), sess.invalidated, hub.reached())
		}
	}
}

func TestOperatorViewHubFailure(t *testing.T) {
	for _, answer := range []func(http.ResponseWriter){
		func(w http.ResponseWriter) { w.WriteHeader(http.StatusForbidden) },
		func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
		func(w http.ResponseWriter) { _, _ = io.WriteString(w, "not json") },
		func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) }, // twice: the retry also fails
	} {
		hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
		hub.answer = func(w http.ResponseWriter, _ *http.Request) bool { answer(w); return true }
		rw := viewDo(viewHandler(t, viewConfig(t, hub, sess, audit)), "op@x", "GET", viewC)
		if rw.Code != http.StatusBadGateway || strings.TrimSpace(rw.Body.String()) != `{"error":"unavailable"}` {
			t.Fatalf("%d %s", rw.Code, rw.Body)
		}
		if len(hub.reached()) > 2 {
			t.Fatalf("at most one retry: %v", hub.reached())
		}
	}
}

func TestOperatorViewAuditHasNoText(t *testing.T) {
	hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
	cfg := viewConfig(t, hub, sess, audit)
	cfg.MatchAgentMessages, cfg.PeekAgentMessages = refuseMatch(t), peekOf(recordedOnly("recorded"), false)
	h := viewHandler(t, cfg)
	viewMessages(t, viewDo(h, "op@x", "GET", viewC))
	l := audit.last()
	if l.Decision != DecisionOperatorView || l.Contact != "c@x" || l.Agent != "w1" || l.Count == nil || *l.Count != 5 {
		t.Fatalf("audit %+v", l)
	}
	b, _ := json.Marshal(audit.all())
	for _, text := range []string{"SECRET", "recorded", "mine", "agent started"} {
		if strings.Contains(string(b), text) {
			t.Fatalf("an audit line carries message text %q: %s", text, b)
		}
	}
}

// TestOperatorViewStaleBinding: apply bound the contact to a hub user its
// session no longer is (the hub forgot it and the login made a new one).
// The hub refuses the bound key; the view asks who the session is, GET
// only, and answers "not-signed-in" with the fixed hint, never a 502.
func TestOperatorViewStaleBinding(t *testing.T) {
	for name, tc := range map[string]struct {
		me        func(w http.ResponseWriter, r *http.Request)
		status    int
		body, why string
	}{
		"another user": {func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"id":"u-new"}`) },
			http.StatusConflict, `{"error":"not-signed-in","hint":"rebind"}`, "stale-binding"},
		"the bound user": {nil, http.StatusBadGateway, `{"error":"unavailable"}`, "unavailable"},
		"no answer":      {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, http.StatusBadGateway, `{"error":"unavailable"}`, "unavailable"},
		// A redirect is not followed, even to an answer naming another user.
		"a redirect": {func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("to") == "" {
				http.Redirect(w, r, "/api/v1/auth/me?to=1", http.StatusFound)
				return
			}
			_, _ = io.WriteString(w, `{"id":"u-new"}`)
		}, http.StatusBadGateway, `{"error":"unavailable"}`, "unavailable"},
	} {
		hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
		hub.me = tc.me
		hub.answer = func(w http.ResponseWriter, _ *http.Request) bool { w.WriteHeader(http.StatusForbidden); return true }
		rw := viewDo(viewHandler(t, viewConfig(t, hub, sess, audit)), "op@x", "GET", viewC)
		if rw.Code != tc.status || strings.TrimSpace(rw.Body.String()) != tc.body {
			t.Fatalf("%s: %d %s", name, rw.Code, rw.Body)
		}
		if l := audit.last(); l.Reason != tc.why || l.Contact != "c@x" {
			t.Fatalf("%s: audit %+v", name, l)
		}
		hub.mu.Lock()
		me := slices.Clone(hub.meSeen)
		hub.mu.Unlock()
		if len(me) != 1 || me[0] != "GET "+viewCCookie {
			t.Fatalf("%s: auth/me asked %v, want one GET with the contact's session", name, me)
		}
		if got := hub.reached(); len(got) != 1 || !strings.HasPrefix(got[0], "GET "+viewDMPath) {
			t.Fatalf("%s: hub saw %v", name, got)
		}
	}
}

// TestOperatorViewAsksWhoOnlyAfterAFailedRead: a read that works costs one
// request.
func TestOperatorViewAsksWhoOnlyAfterAFailedRead(t *testing.T) {
	hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
	viewMessages(t, viewDo(viewHandler(t, viewConfig(t, hub, sess, audit)), "op@x", "GET", viewC))
	if len(hub.meSeen) != 0 {
		t.Fatalf("auth/me asked %v", hub.meSeen)
	}
}

// TestOperatorViewOversizeAnswer: a history answer over maxHistoryAnswer is
// not read on, and no row of it is answered.
func TestOperatorViewOversizeAnswer(t *testing.T) {
	hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
	big := `{"messages":[{"id":"a1","sender":"agent:w1","senderId":"id-w1","type":"instruction","msg":"`
	big += strings.Repeat("x", maxHistoryAnswer+1-len(big)-3) + `"}]`
	hub.answer = func(w http.ResponseWriter, _ *http.Request) bool { _, _ = io.WriteString(w, big+"}"); return true }
	rw := viewDo(viewHandler(t, viewConfig(t, hub, sess, audit)), "op@x", "GET", viewC)
	if rw.Code != http.StatusBadGateway || strings.TrimSpace(rw.Body.String()) != `{"error":"unavailable"}` {
		t.Fatalf("%d %.200s", rw.Code, rw.Body)
	}
	if l := audit.last(); l.Reason != "unavailable" || l.Count != nil {
		t.Fatalf("audit %+v", l)
	}
}

// Each history failure leaves its cause on the audit line: bounded, and
// never the hub's body or the contact's session.
func TestOperatorViewFailureHasACause(t *testing.T) {
	const body = "SECRET hub body"
	status := func(code int) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) { w.WriteHeader(code); _, _ = io.WriteString(w, body) }
	}
	for name, tc := range map[string]struct {
		answer  func(http.ResponseWriter)
		records func(context.Context) (map[string]AgentRecord, error)
		closed  bool
		want    string
	}{
		"forbidden":  {answer: status(http.StatusForbidden), want: "HTTP 403"},
		"hub error":  {answer: status(http.StatusInternalServerError), want: "HTTP 500"},
		"lapsed":     {answer: status(http.StatusUnauthorized), want: "HTTP 401"},
		"not json":   {answer: func(w http.ResponseWriter) { _, _ = io.WriteString(w, body) }, want: "unreadable"},
		"too big":    {answer: func(w http.ResponseWriter) { _, _ = io.WriteString(w, strings.Repeat("x", maxHistoryAnswer+1)) }, want: "too big"},
		"no hub":     {closed: true, want: "hub history: "},
		"no records": {records: func(context.Context) (map[string]AgentRecord, error) { return nil, errors.New(body) }, want: "agent records"},
		"bad id": {records: func(context.Context) (map[string]AgentRecord, error) {
			return map[string]AgentRecord{"w1": {ID: "../" + body}}, nil
		}, want: "hub id"},
	} {
		t.Run(name, func(t *testing.T) {
			hub, sess, audit := newViewHub(t), newLoginSession(), &viewAudit{}
			if tc.answer != nil {
				hub.answer = func(w http.ResponseWriter, _ *http.Request) bool { tc.answer(w); return true }
			}
			cfg := viewConfig(t, hub, sess, audit)
			if tc.records != nil {
				cfg.AgentRecords = tc.records
			}
			h := viewHandler(t, cfg)
			if tc.closed {
				hub.Close()
			}
			rw := viewDo(h, "op@x", "GET", viewC)
			if rw.Code != http.StatusBadGateway {
				t.Fatalf("%d %s", rw.Code, rw.Body)
			}
			l := audit.last()
			if l.Reason != "unavailable" || !strings.Contains(l.Error, tc.want) || len(l.Error) > maxAuditFieldLen+len("…") ||
				strings.Contains(l.Error, "SECRET") || strings.Contains(l.Error, viewCCookie) {
				t.Fatalf("audit %+v, want a cause with %q", l, tc.want)
			}
		})
	}
}
