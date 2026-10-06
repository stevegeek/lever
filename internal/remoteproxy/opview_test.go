package remoteproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
// /api/v1/auth/me (the contact fence's own question) is answered with the
// contact's id and not recorded: the operator view never asks it.
type viewHub struct {
	*httptest.Server
	mu     sync.Mutex
	seen   []string
	answer func(w http.ResponseWriter, r *http.Request) bool
}

const viewDMPath = "/api/v1/chat/conversations/dm:agent:" + agentW1 + ":user:" + contactUID + "/messages"

func newViewHub(t *testing.T) *viewHub {
	h := &viewHub{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/me" {
			_, _ = io.WriteString(w, `{"id":"`+contactUID+`"}`)
			return
		}
		cookie := ""
		if c, err := r.Cookie(sessionCookieName); err == nil {
			cookie = c.Value
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
