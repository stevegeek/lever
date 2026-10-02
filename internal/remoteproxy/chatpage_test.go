package remoteproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stevegeek/lever/internal/chatledger"
)

const (
	chatOp    = "op@x"
	chatUID   = "u-op"
	chatMgrID = "id-mgr"
)

// pageHub is a hub for the chat page tests: /api/v1/auth/me names the
// operator, everything else is recorded and answered 200 {}.
type pageHub struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
	me   func(w http.ResponseWriter, r *http.Request)
}

func newPageHub(t *testing.T) *pageHub {
	h := &pageHub{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/me" {
			h.mu.Lock()
			me := h.me
			h.mu.Unlock()
			if me != nil {
				me(w, r)
				return
			}
			_, _ = io.WriteString(w, `{"id":"`+chatUID+`"}`)
			return
		}
		h.mu.Lock()
		h.seen = append(h.seen, r.Method+" "+r.URL.RequestURI())
		h.mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *pageHub) reached() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

// chatConfig is a proxy with the chat page on for manager "boss", one
// operator and one contact.
func chatConfig(t *testing.T, hub *pageHub) Config {
	t.Helper()
	return Config{Target: mustURL(t, hub.URL), Session: testSession(), ServeHost: testServeHost,
		AllowedUsers: []string{chatOp, "c@x"}, Contacts: map[string][]string{"c@x": {"w1"}},
		ResolveAgents: func(context.Context) (map[string]string, error) {
			return map[string]string{"w1": agentW1, "boss": chatMgrID}, nil
		},
		ContactSession: func(string) error { return nil },
		ChatAgent:      "boss"}
}

func chatDo(h http.Handler, login, method, target string, hdr ...string) *httptest.ResponseRecorder {
	req := proxyRequest(method, target, nil)
	if login != "" {
		req.Header.Set("Tailscale-User-Login", login)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	return rw
}

// TestChatPageServesItsFiles: an operator gets the page and its files from
// the binary, with the page's CSP, and the hub is never asked for them.
func TestChatPageServesItsFiles(t *testing.T) {
	hub := newPageHub(t)
	h := NewHandler(chatConfig(t, hub))
	for path, ctype := range map[string]string{
		"/lever/chat":        "text/html; charset=utf-8",
		"/lever/chat.css":    "text/css; charset=utf-8",
		"/lever/chat.js":     "text/javascript; charset=utf-8",
		"/lever/chatcore.js": "text/javascript; charset=utf-8",
	} {
		rw := chatDo(h, chatOp, "GET", path)
		if rw.Code != http.StatusOK || rw.Header().Get("Content-Type") != ctype || rw.Body.Len() == 0 {
			t.Errorf("%s: %d %q (%d bytes), want 200 %s", path, rw.Code, rw.Header().Get("Content-Type"), rw.Body.Len(), ctype)
		}
		if got := rw.Header().Get("Content-Security-Policy"); got != chatCSP {
			t.Errorf("%s: CSP %q", path, got)
		}
		for k, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY",
			"Referrer-Policy": "no-referrer", "Cache-Control": "no-cache"} {
			if got := rw.Header().Get(k); got != want {
				t.Errorf("%s: %s = %q, want %q", path, k, got, want)
			}
		}
		etag := rw.Header().Get("ETag")
		if etag == "" {
			t.Errorf("%s: no ETag", path)
		}
		if again := chatDo(h, chatOp, "GET", path, "If-None-Match", etag); again.Code != http.StatusNotModified || again.Body.Len() != 0 {
			t.Errorf("%s: revalidation answered %d with %d bytes, want 304 and none", path, again.Code, again.Body.Len())
		}
		if head := chatDo(h, chatOp, "HEAD", path); head.Code != http.StatusOK || head.Body.Len() != 0 {
			t.Errorf("%s: HEAD answered %d with %d bytes", path, head.Code, head.Body.Len())
		}
	}
	if got := hub.reached(); len(got) != 0 {
		t.Fatalf("the hub was asked for lever's own files: %v", got)
	}
}

// TestChatPageCSPAllowsNoInlineCode: the policy is what makes agent text
// that slipped into the page inert, so its load-bearing parts are pinned.
func TestChatPageCSPAllowsNoInlineCode(t *testing.T) {
	for _, bad := range []string{"unsafe-inline", "unsafe-eval", "*", "data:", "blob:", "http:", "https:"} {
		if strings.Contains(chatCSP, bad) {
			t.Errorf("chat CSP contains %q: %s", bad, chatCSP)
		}
	}
	for _, need := range []string{"default-src 'none'", "script-src 'self'", "base-uri 'none'", "form-action 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(chatCSP, need) {
			t.Errorf("chat CSP lacks %q: %s", need, chatCSP)
		}
	}
}

// TestChatPageLanding: "/" goes to the page, with a fixed target whatever
// the request carries, and only for a read.
func TestChatPageLanding(t *testing.T) {
	hub := newPageHub(t)
	h := NewHandler(chatConfig(t, hub))
	for _, target := range []string{"/", "/?returnTo=//evil.test", "/?x=https://evil.test/lever/chat"} {
		for _, method := range []string{"GET", "HEAD"} {
			rw := chatDo(h, chatOp, method, target)
			if rw.Code != http.StatusFound || rw.Header().Get("Location") != chatPagePath {
				t.Errorf("%s %s: %d Location %q, want 302 %s", method, target, rw.Code, rw.Header().Get("Location"), chatPagePath)
			}
		}
	}
	if got := hub.reached(); len(got) != 0 {
		t.Fatalf("a landing redirect reached the hub: %v", got)
	}
	// Not a read, or not exactly "/": the hub's, as before.
	for _, c := range [][2]string{{"POST", "/"}, {"GET", "/agents"}, {"GET", "/chat"}, {"GET", "/leverage"}, {"GET", "/api/v1/agents"}} {
		rw := chatDo(h, chatOp, c[0], c[1])
		if rw.Code != http.StatusOK || rw.Header().Get("Location") != "" {
			t.Errorf("%s %s: %d, want it forwarded", c[0], c[1], rw.Code)
		}
	}
	if got := strings.Join(hub.reached(), ","); got != "POST /,GET /agents,GET /chat,GET /leverage,GET /api/v1/agents" {
		t.Fatalf("forwarded %q", got)
	}
}

// TestChatPageOwnsItsPrefix: an unknown path under /lever/ is a 404 from the
// proxy and a write is refused; neither reaches the hub.
func TestChatPageOwnsItsPrefix(t *testing.T) {
	hub := newPageHub(t)
	h := NewHandler(chatConfig(t, hub))
	for _, p := range []string{"/lever", "/lever/", "/lever/nope", "/lever/chat/", "/lever/chat.html", "/lever/chatui/chat.js",
		"/lever/../api/v1/agents", "/lever/api/", "/lever/api/chat/x", "/lever/chatcore.test.mjs", "/lever/package.json"} {
		rw := chatDo(h, chatOp, "GET", p)
		if rw.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, rw.Code)
		}
		if rw.Header().Get("Content-Security-Policy") != chatCSP || rw.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: the 404 lacks the page's headers", p)
		}
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		for _, p := range []string{chatPagePath, chatBootstrapPath, "/lever/chat.js"} {
			if rw := chatDo(h, chatOp, m, p); rw.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: %d, want 405", m, p, rw.Code)
			}
		}
	}
	if got := hub.reached(); len(got) != 0 {
		t.Fatalf("a /lever/ request reached the hub: %v", got)
	}
}

// TestChatPageOffLeavesEveryPathToTheHub: without ChatAgent nothing changes.
func TestChatPageOffLeavesEveryPathToTheHub(t *testing.T) {
	hub := newPageHub(t)
	cfg := chatConfig(t, hub)
	cfg.ChatAgent = ""
	h := NewHandler(cfg)
	paths := []string{"/", chatPagePath, "/lever/chat.js", chatBootstrapPath, "/lever/nope"}
	for _, p := range paths {
		if rw := chatDo(h, chatOp, "GET", p); rw.Code != http.StatusOK || rw.Body.String() != "{}" {
			t.Errorf("GET %s: %d %q, want the hub's answer", p, rw.Code, rw.Body)
		}
	}
	if got := hub.reached(); len(got) != len(paths) {
		t.Fatalf("the hub saw %v, want all of %v", got, paths)
	}
}

// TestChatPageNeedsAVerifiedLogin: with no allowed_users nobody is verified,
// so there is no page even when ChatAgent is set.
func TestChatPageNeedsAVerifiedLogin(t *testing.T) {
	hub := newPageHub(t)
	cfg := chatConfig(t, hub)
	cfg.AllowedUsers, cfg.Contacts = nil, nil
	h := NewHandler(cfg)
	for _, p := range []string{"/", chatPagePath, chatBootstrapPath} {
		if rw := chatDo(h, chatOp, "GET", p); rw.Code != http.StatusOK || rw.Body.String() != "{}" {
			t.Errorf("GET %s: %d %q, want it forwarded", p, rw.Code, rw.Body)
		}
	}
}

// TestChatPageIsNotForContacts: a contact gets the fence's landing page for
// every one of the chat page's paths, never the page, its script or its data.
func TestChatPageIsNotForContacts(t *testing.T) {
	hub := newPageHub(t)
	h := NewHandler(chatConfig(t, hub))
	for _, p := range []string{"/", chatPagePath, "/lever/chat.js", "/lever/chatcore.js", "/lever/chat.css", chatBootstrapPath} {
		rw := chatDo(h, "c@x", "GET", p)
		body := rw.Body.String()
		if rw.Code != http.StatusOK || !strings.Contains(body, "<h1>Chat</h1>") {
			t.Errorf("GET %s as a contact: %d %q, want the contact landing page", p, rw.Code, body)
		}
		for _, leak := range []string{chatMgrID, "boss", chatOp, "chatcore", "userId", "EventSource"} {
			if strings.Contains(body, leak) {
				t.Errorf("GET %s as a contact leaks %q", p, leak)
			}
		}
		if rw.Header().Get("Location") != "" {
			t.Errorf("GET %s as a contact was redirected to %q", p, rw.Header().Get("Location"))
		}
	}
	for _, p := range []string{chatPagePath, chatBootstrapPath} {
		if rw := chatDo(h, "c@x", "POST", p); rw.Code != http.StatusForbidden {
			t.Errorf("POST %s as a contact: %d, want 403", p, rw.Code)
		}
	}
}

// TestChatPageStaysBehindTheGate: the page is answered only after the same
// checks as every forwarded request.
func TestChatPageStaysBehindTheGate(t *testing.T) {
	hub := newPageHub(t)
	h := NewHandler(chatConfig(t, hub))
	for name, hdr := range map[string][]string{
		"unknown login":      {"Tailscale-User-Login", "stranger@x"},
		"cross origin":       {"Origin", "https://evil.test"},
		"cross site":         {"Sec-Fetch-Site", "cross-site"},
		"same-site sibling":  {"Sec-Fetch-Site", "same-site"},
		"comma-joined login": {"Tailscale-User-Login", "evil@x, " + chatOp},
	} {
		for _, p := range []string{"/", chatPagePath, "/lever/chat.js", chatBootstrapPath} {
			login := chatOp
			if hdr[0] == "Tailscale-User-Login" {
				login = ""
			}
			if rw := chatDo(h, login, "GET", p, hdr...); rw.Code != http.StatusForbidden {
				t.Errorf("%s, GET %s: %d, want 403", name, p, rw.Code)
			}
		}
	}
	// No login header at all, and a foreign Host.
	if rw := chatDo(h, "", "GET", chatBootstrapPath); rw.Code != http.StatusForbidden {
		t.Errorf("no login: %d, want 403", rw.Code)
	}
	req := proxyRequest("GET", chatBootstrapPath, nil)
	req.Host = "evil.test"
	req.Header.Set("Tailscale-User-Login", chatOp)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Errorf("foreign Host: %d, want 403", rw.Code)
	}
	// And no page without a hub session.
	cfg := chatConfig(t, hub)
	cfg.Session = &stubSession{err: errors.New("login failed")}
	if rw := chatDo(NewHandler(cfg), chatOp, "GET", chatPagePath); rw.Code != http.StatusBadGateway {
		t.Errorf("no hub session: %d, want 502", rw.Code)
	}
}

// TestChatBootstrap: the answer names the operator, the manager and their
// conversation, and is not cacheable.
func TestChatBootstrap(t *testing.T) {
	hub := newPageHub(t)
	h := NewHandler(chatConfig(t, hub))
	rw := chatDo(h, chatOp, "GET", chatBootstrapPath)
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d %s", rw.Code, rw.Body)
	}
	want := `{"login":"op@x","userId":"u-op","agent":{"name":"boss","id":"id-mgr"},` +
		`"conversation":"dm:agent:id-mgr:user:u-op","terminal":"/agents/id-mgr/terminal","console":"/agents"}`
	if got := rw.Body.String(); got != want {
		t.Fatalf("bootstrap\n got %s\nwant %s", got, want)
	}
	for k, v := range map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store",
		"Content-Security-Policy": "sandbox", "X-Content-Type-Options": "nosniff"} {
		if got := rw.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if head := chatDo(h, chatOp, "HEAD", chatBootstrapPath); head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD: %d with %d bytes", head.Code, head.Body.Len())
	}
	// The conversation it names is one recordChat records.
	if _, agent, ok := chatDMAgent("POST", chatConversationsPrefix+"dm:agent:id-mgr:user:u-op/messages"); !ok || agent != chatMgrID {
		t.Fatal("the bootstrap's conversation key is not a recorded DM route")
	}
}

// TestChatBootstrapWithoutAManagerRecord: no hub record is a state the page
// reports, not a fault.
func TestChatBootstrapWithoutAManagerRecord(t *testing.T) {
	hub := newPageHub(t)
	cfg := chatConfig(t, hub)
	cfg.ResolveAgents = func(context.Context) (map[string]string, error) { return map[string]string{"w1": agentW1}, nil }
	rw := chatDo(NewHandler(cfg), chatOp, "GET", chatBootstrapPath)
	want := `{"login":"op@x","userId":"u-op","agent":{"name":"boss","id":""},"console":"/agents"}`
	if rw.Code != http.StatusOK || rw.Body.String() != want {
		t.Fatalf("%d %s, want 200 %s", rw.Code, rw.Body, want)
	}
}

// TestChatBootstrapFailsClosed: an identity or agent the proxy cannot
// resolve, or an id that is not a plain token, gets no answer built from it.
func TestChatBootstrapFailsClosed(t *testing.T) {
	ok := func(context.Context) (map[string]string, error) { return map[string]string{"boss": chatMgrID}, nil }
	me := func(body string, status int) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}
	}
	ids := func(id string) func(context.Context) (map[string]string, error) {
		return func(context.Context) (map[string]string, error) { return map[string]string{"boss": id}, nil }
	}
	for name, c := range map[string]struct {
		me      func(http.ResponseWriter, *http.Request)
		resolve func(context.Context) (map[string]string, error)
	}{
		"resolver error":      {nil, func(context.Context) (map[string]string, error) { return nil, errors.New("hub down") }},
		"no resolver":         {nil, nil},
		"hub 500 on identity": {me(`{}`, 500), ok},
		"no user id":          {me(`{"id":""}`, 200), ok},
		"identity not JSON":   {me(`<html>`, 200), ok},
		"user id with slash":  {me(`{"id":"u/../../x"}`, 200), ok},
		"user id with colon":  {me(`{"id":"u:agent:x"}`, 200), ok},
		"user id with quote":  {me(`{"id":"u\"x"}`, 200), ok},
		"user id with space":  {me(`{"id":"u x"}`, 200), ok},
		"user id too long":    {me(`{"id":"`+strings.Repeat("a", 129)+`"}`, 200), ok},
		"agent id with slash": {nil, ids("a/terminal/../../x")},
		"agent id with colon": {nil, ids("a:user:other")},
		"agent id with dots":  {nil, ids("..")},
		"agent id with query": {nil, ids("a?x=1")},
		"agent id non-ASCII":  {nil, ids("aé")},
	} {
		hub := newPageHub(t)
		hub.me = c.me
		cfg := chatConfig(t, hub)
		cfg.ResolveAgents = c.resolve
		var lines []AuditLine
		cfg.Audit = func(l AuditLine) { lines = append(lines, l) }
		rw := chatDo(NewHandler(cfg), chatOp, "GET", chatBootstrapPath)
		if rw.Code != http.StatusBadGateway {
			t.Errorf("%s: %d %s, want 502", name, rw.Code, rw.Body)
		}
		if strings.Contains(rw.Body.String(), "conversation") || strings.Contains(rw.Body.String(), "{") {
			t.Errorf("%s: a refused bootstrap still carries data: %s", name, rw.Body)
		}
		if len(lines) != 1 || lines[0].Decision != DecisionChatUnavailable || lines[0].Status != http.StatusBadGateway {
			t.Errorf("%s: audit %+v, want one chat-unavailable 502", name, lines)
		}
	}
}

func TestValidHubID(t *testing.T) {
	for _, good := range []string{"a", "id-mgr", "0191f2c4-7b1e-7c3a-9d2e-3f4a5b6c7d8e", "A_b-9", strings.Repeat("a", 128)} {
		if !validHubID(good) {
			t.Errorf("validHubID(%q) = false", good)
		}
	}
	for _, bad := range []string{"", " ", "a b", "a/b", "a:b", "a.b", "..", "a%2f", "a\n", "a\x00", "é", "a\"", "a'", "<a>", strings.Repeat("a", 129)} {
		if validHubID(bad) {
			t.Errorf("validHubID(%q) = true", bad)
		}
	}
}

// TestChatBootstrapHealsALapsedSession: a 401 from the identity lookup
// replaces the session once.
func TestChatBootstrapHealsALapsedSession(t *testing.T) {
	hub := newPageHub(t)
	hub.me = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != sessionCookieName+"=new" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"id":"`+chatUID+`"}`)
	}
	cfg := chatConfig(t, hub)
	cfg.Session = &rotatingSession{}
	rw := chatDo(NewHandler(cfg), chatOp, "GET", chatBootstrapPath)
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), `"userId":"u-op"`) {
		t.Fatalf("%d %s, want the bootstrap on the new session", rw.Code, rw.Body)
	}
}

// TestChatPageAuditsEveryAnswer: one line per request, as for a forwarded one.
func TestChatPageAuditsEveryAnswer(t *testing.T) {
	hub := newPageHub(t)
	cfg := chatConfig(t, hub)
	var mu sync.Mutex
	var lines []AuditLine
	cfg.Audit = func(l AuditLine) { mu.Lock(); lines = append(lines, l); mu.Unlock() }
	h := NewHandler(cfg)
	for _, c := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/", http.StatusFound},
		{"GET", chatPagePath, http.StatusOK},
		{"GET", "/lever/chat.js", http.StatusOK},
		{"GET", chatBootstrapPath, http.StatusOK},
		{"GET", "/lever/nope", http.StatusNotFound},
		{"POST", chatPagePath, http.StatusMethodNotAllowed},
	} {
		lines = nil
		chatDo(h, chatOp, c.method, c.path)
		if len(lines) != 1 || lines[0].Decision != DecisionAllow || lines[0].Status != c.status || lines[0].Path != c.path || lines[0].TSLogin != chatOp {
			t.Errorf("%s %s: audit %+v, want one allow line with status %d", c.method, c.path, lines, c.status)
		}
	}
}

// TestChatPagePostIsStillRecorded: what the page sends goes through the
// ordinary forward path, so the chat ledger records it as the operator's.
func TestChatPagePostIsStillRecorded(t *testing.T) {
	const key = "dm:agent:id-mgr:user:u-op"
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"m1","content":"hello","sender":"user:op","createdAt":"2026-10-02T10:00:00Z"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"`+chatUID+`"}`)
	}))
	t.Cleanup(hub.Close)
	var got []chatledger.Entry
	cfg := Config{Target: mustURL(t, hub.URL), Session: testSession(), ServeHost: testServeHost,
		AllowedUsers:  []string{chatOp},
		ResolveAgents: func(context.Context) (map[string]string, error) { return map[string]string{"boss": chatMgrID}, nil },
		ChatAgent:     "boss",
		ChatLedger:    func(e chatledger.Entry) error { got = append(got, e); return nil }}
	h := NewHandler(cfg)

	var boot chatBootstrap
	if err := json.Unmarshal(chatDo(h, chatOp, "GET", chatBootstrapPath).Body.Bytes(), &boot); err != nil || boot.Conversation != key {
		t.Fatalf("bootstrap conversation %q (%v), want %s", boot.Conversation, err, key)
	}
	req := proxyRequest("POST", chatConversationsPrefix+boot.Conversation+"/messages", strings.NewReader(`{"content":"hello"}`))
	req.Header.Set("Tailscale-User-Login", chatOp)
	req.Header.Set("Origin", "https://"+testServeHost)
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", rw.Code, rw.Body)
	}
	if len(got) != 1 || got[0].Login != chatOp || got[0].Tier != chatledger.TierOperator || got[0].AgentID != chatMgrID ||
		got[0].Conversation != key || got[0].MessageID != "m1" || got[0].Text != "hello" {
		t.Fatalf("ledger %+v, want one operator entry for the manager DM", got)
	}
}

// TestChatPageHasNoMarkupSink: the page shows agent text on the operator's
// origin, so its script may only ever write text. This fails on the ways a
// later edit could turn text into markup or code.
func TestChatPageHasNoMarkupSink(t *testing.T) {
	sinks := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|\beval\s*\(|new\s+Function|setTimeout\s*\(\s*['"\x60]|setInterval\s*\(\s*['"\x60]|srcdoc|javascript:|createContextualFragment|DOMParser|\.setHTML|import\s*\(|\.src\s*=|location\s*(\.href)?\s*=`)
	for _, name := range []string{"chatui/chat.js", "chatui/chatcore.js"} {
		b, err := chatUI.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if m := sinks.Find(b); m != nil {
			t.Errorf("%s contains %q: the chat page writes network text as text only", name, m)
		}
	}
	page, err := chatUI.ReadFile("chatui/chat.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	for _, re := range []string{`(?i)<script(?:\s[^>]*)?>\s*[^<\s]`, `(?i)\son[a-z]+\s*=`, `(?i)\sstyle\s*=`, `(?i)<style`, `(?i)<iframe|<object|<embed|<base`,
		`(?i)(?:src|href)\s*=\s*["']?(?:https?:)?//`} {
		if m := regexp.MustCompile(re).FindString(html); m != "" {
			t.Errorf("chat.html matches %s (%q): no inline code, no inline style, no other origin", re, m)
		}
	}
	css, err := chatUI.ReadFile("chatui/chat.css")
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`(?i)url\s*\(|@import|expression\s*\(`).Find(css); m != nil {
		t.Errorf("chat.css contains %q: it loads nothing", m)
	}
}

// TestChatCoreJS runs the page logic's own tests when node is installed.
func TestChatCoreJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; chatui/chatcore.test.mjs not run")
	}
	cmd := exec.Command(node, "--test", "chatcore.test.mjs")
	cmd.Dir = "chatui"
	cmd.Env = append(os.Environ(), "NODE_NO_WARNINGS=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}
