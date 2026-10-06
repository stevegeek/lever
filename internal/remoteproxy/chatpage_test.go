package remoteproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	// route, when set, answers a request it reports true for (after it is
	// recorded); the chat list's DM and history reads use it.
	route func(w http.ResponseWriter, r *http.Request) bool
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
		route := h.route
		h.mu.Unlock()
		if route != nil && route(w, r) {
			return
		}
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
		ChatAgent:      "boss",
		Workers:        []string{"w1", "w2", "w3"},
		ContactSee:     map[string][]string{"c@x": {"w2"}},
		AgentRecords: func(context.Context) (map[string]AgentRecord, error) {
			return map[string]AgentRecord{"boss": {ID: chatMgrID, Phase: "running"}, "w1": {ID: agentW1, Phase: "running"},
				"w2": {ID: "id-w2", Phase: "running"}, "w3": {ID: "id-w3", Phase: "suspended"}}, nil
		}}
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
		"/lever/chat":                  "text/html; charset=utf-8",
		"/lever/chat.css":              "text/css; charset=utf-8",
		"/lever/chat.js":               "text/javascript; charset=utf-8",
		"/lever/chatcore.js":           "text/javascript; charset=utf-8",
		chatManifestPath:               "application/manifest+json",
		"/lever/icon-192.png":          "image/png",
		"/lever/icon-512.png":          "image/png",
		"/lever/icon-maskable-512.png": "image/png",
		"/lever/apple-touch-icon.png":  "image/png",
	} {
		rw := chatDo(h, chatOp, "GET", path)
		if rw.Code != http.StatusOK || rw.Header().Get("Content-Type") != ctype || rw.Body.Len() == 0 {
			t.Errorf("%s: %d %q (%d bytes), want 200 %s", path, rw.Code, rw.Header().Get("Content-Type"), rw.Body.Len(), ctype)
		}
		if got := rw.Header().Get("Content-Security-Policy"); got != chatCSPFor(testServeHost) {
			t.Errorf("%s: CSP %q", path, got)
		}
		for k, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY",
			"Referrer-Policy": "no-referrer", "Cache-Control": "private, no-cache"} {
			if got := rw.Header().Get(k); got != want {
				t.Errorf("%s: %s = %q, want %q", path, k, got, want)
			}
		}
		etag := rw.Header().Get("ETag")
		if etag == "" {
			t.Errorf("%s: no ETag", path)
		}
		// A front that compresses weakens the tag; a client may send a list.
		for _, inm := range []string{etag, "W/" + etag, `"other", ` + etag, `"other",W/` + etag, "*"} {
			again := chatDo(h, chatOp, "GET", path, "If-None-Match", inm)
			if again.Code != http.StatusNotModified || again.Body.Len() != 0 {
				t.Errorf("%s: If-None-Match %s answered %d with %d bytes, want 304 and none", path, inm, again.Code, again.Body.Len())
			}
			if h := again.Header(); h.Get("ETag") != etag || h.Get("Cache-Control") != "private, no-cache" || h.Get("Content-Type") != ctype {
				t.Errorf("%s: the 304 lacks the file's headers: %v", path, h)
			}
		}
		for _, inm := range []string{`"other"`, "", ",", strings.Trim(etag, `"`), `W/"other"`, `"x` + etag[1:], etag + `x`, `x` + etag, `"*"`, "W/*"} {
			if again := chatDo(h, chatOp, "GET", path, "If-None-Match", inm); again.Code != http.StatusOK {
				t.Errorf("%s: If-None-Match %q answered %d, want 200", path, inm, again.Code)
			}
		}
		if head := chatDo(h, chatOp, "HEAD", path); head.Code != http.StatusOK || head.Body.Len() != 0 {
			t.Errorf("%s: HEAD answered %d with %d bytes", path, head.Code, head.Body.Len())
		}
	}
	if got := hub.reached(); len(got) != 0 {
		t.Fatalf("the hub was asked for lever's own files: %v", got)
	}
}

// TestChatPageCSPAllowsNoInlineCode: the policy is what keeps text that
// slipped into the page from running, so its load-bearing parts are pinned.
// Script and style come from /lever/ only: on this origin 'self' would also
// admit a .js file an agent wrote, which the hub serves under /api/.
func TestChatPageCSPAllowsNoInlineCode(t *testing.T) {
	csp := chatCSPFor("mac.ts.net")
	for _, bad := range []string{"unsafe-inline", "unsafe-eval", "*", "data:", "blob:", "http:", "https:", "script-src 'self'", "style-src 'self'"} {
		if strings.Contains(csp, bad) {
			t.Errorf("chat CSP contains %q: %s", bad, csp)
		}
	}
	for _, need := range []string{"default-src 'none'", "script-src mac.ts.net/lever/;", "style-src mac.ts.net/lever/;",
		"img-src mac.ts.net/favicon.svg mac.ts.net/lever/;", "manifest-src mac.ts.net/lever/;", "worker-src mac.ts.net/lever/;", "connect-src 'self'", "base-uri 'none'", "form-action 'none'", "frame-ancestors 'none'",
		"require-trusted-types-for 'script'", "trusted-types 'none'"} {
		if !strings.Contains(csp, need) {
			t.Errorf("chat CSP lacks %q: %s", need, csp)
		}
	}
	// The whole policy, pinned: a directive added or widened shows up here.
	if want := "default-src 'none'; script-src mac.ts.net/lever/; style-src mac.ts.net/lever/; connect-src 'self'; " +
		"img-src mac.ts.net/favicon.svg mac.ts.net/lever/; manifest-src mac.ts.net/lever/; worker-src mac.ts.net/lever/; base-uri 'none'; form-action 'none'; " +
		"frame-ancestors 'none'; require-trusted-types-for 'script'; trusted-types 'none'"; csp != want {
		t.Errorf("chat CSP\n got %s\nwant %s", csp, want)
	}
	if got := chatCSPFor("127.0.0.1:8445"); !strings.Contains(got, "script-src 127.0.0.1:8445/lever/;") {
		t.Errorf("an address with a port must be nameable: %s", got)
	}
	// A host the policy cannot name is never written into it. An IPv6
	// literal is one: CSP has no bracket form, and a source the browser
	// cannot parse would leave the page with no script.
	for _, odd := range []string{"", "a b", "a;script-src *", "a,b", "a'b", "a/b", "*", "*.ts.net", "[::1]:8445", "[fd7a::1]", "fd7a::1",
		"https:", "data:", "a:", ":80", "a:b", "a:80:90", strings.Repeat("a", 256)} {
		got := chatCSPFor(odd)
		if !strings.Contains(got, "script-src 'self';") || !strings.Contains(got, "img-src 'self'; manifest-src 'self';") ||
			(odd != "" && strings.Contains(got, odd)) {
			t.Errorf("chatCSPFor(%q) = %s, want the 'self' fallback", odd, got)
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
			// An operator and a contact get different answers for "/".
			if got := rw.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("%s %s: Cache-Control %q, want no-store", method, target, got)
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
		"/lever/../api/v1/agents", "/lever/api/", "/lever/api/chat", "/lever/api/chat/x", "/lever/api/agents/", "/lever/api/agents/w1", "/lever/api/agentsx", "/lever/chatcore.test.mjs", "/lever/chat.test.mjs", "/lever/chatcss.test.mjs",
		"/lever/fakebrowser.mjs", "/lever/package.json"} {
		rw := chatDo(h, chatOp, "GET", p)
		if rw.Code != http.StatusNotFound || rw.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s: %d (Cache-Control %q), want an uncached 404", p, rw.Code, rw.Header().Get("Cache-Control"))
		}
		if rw.Header().Get("Content-Security-Policy") != chatCSPFor(testServeHost) || rw.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: the 404 lacks the page's headers", p)
		}
		if head := chatDo(h, chatOp, "HEAD", p); head.Code != http.StatusNotFound || head.Body.Len() != 0 {
			t.Errorf("HEAD %s: %d with %d bytes, want a bodiless 404", p, head.Code, head.Body.Len())
		}
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		for _, p := range []string{chatPagePath, chatAgentsPath, "/lever/chat.js", chatManifestPath, "/lever/icon-192.png"} {
			if rw := chatDo(h, chatOp, m, p); rw.Code != http.StatusMethodNotAllowed || rw.Header().Get("Allow") != "GET, HEAD" {
				t.Errorf("%s %s: %d Allow %q, want 405 and the allowed methods", m, p, rw.Code, rw.Header().Get("Allow"))
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
	paths := []string{"/", chatPagePath, "/lever/chat.js", chatAgentsPath, "/lever/nope"}
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
	for _, p := range []string{"/", chatPagePath, chatAgentsPath} {
		if rw := chatDo(h, chatOp, "GET", p); rw.Code != http.StatusOK || rw.Body.String() != "{}" {
			t.Errorf("GET %s: %d %q, want it forwarded", p, rw.Code, rw.Body)
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
		for _, p := range []string{"/", chatPagePath, "/lever/chat.js", chatAgentsPath, chatManifestPath, "/lever/icon-512.png"} {
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
	if rw := chatDo(h, "", "GET", chatAgentsPath); rw.Code != http.StatusForbidden {
		t.Errorf("no login: %d, want 403", rw.Code)
	}
	req := proxyRequest("GET", chatAgentsPath, nil)
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
		{"GET", chatAgentsPath, http.StatusOK},
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
		AgentRecords: func(context.Context) (map[string]AgentRecord, error) {
			return map[string]AgentRecord{"boss": {ID: chatMgrID, Phase: "running"}}, nil
		},
		ChatAgent:  "boss",
		ChatLedger: func(e chatledger.Entry) error { got = append(got, e); return nil }}
	h := NewHandler(cfg)

	var list agentsAnswer
	if err := json.Unmarshal(chatDo(h, chatOp, "GET", chatAgentsPath).Body.Bytes(), &list); err != nil || len(list.Agents) == 0 || list.Agents[0].Conversation != key {
		t.Fatalf("agent list %+v (%v), want the manager's conversation %s first", list, err, key)
	}
	// The conversation it names is one recordChat records.
	if _, agent, ok := chatDMAgent("POST", chatConversationsPrefix+key+"/messages"); !ok || agent != chatMgrID {
		t.Fatal("the list's conversation key is not a recorded DM route")
	}
	req := proxyRequest("POST", chatConversationsPrefix+list.Agents[0].Conversation+"/messages", strings.NewReader(`{"content":"hello"}`))
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

// chatSinkRE matches the ways a later edit could turn text into markup or
// code. open( is a page-level or window.open call; a method named open of
// another object (XMLHttpRequest.open, the file upload) is not one.
var chatSinkRE = regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|\beval\s*\(|new\s+Function|setTimeout\s*\(\s*['"\x60]|setInterval\s*\(\s*['"\x60]|srcdoc|javascript:|createContextualFragment|DOMParser|\.setHTML|parseHTMLUnsafe|import\s*\(|\.src\s*=|\.href\s*=|location\s*(\.href)?\s*=|location\.(assign|replace)|(?:^|[^.\w$])open\s*\(|\bwindow\s*\.\s*open\s*\(|setAttribute\(\s*['"\x60](on|style|src)|\[\s*['"\x60][^\]]*\+`)

// TestChatPageHasNoMarkupSink: the page shows agent text on the operator's
// origin, so its script may only ever write text. This fails on the ways a
// later edit could turn text into markup or code.
// The narrowed open( rule still catches a page-level open and window.open,
// and lets a method of another object (XMLHttpRequest.open) through.
func TestMarkupSinkRuleOpen(t *testing.T) {
	for src, bad := range map[string]bool{"open('x')": true, "window.open('x')": true, " open (u)": true, "window . open(u)": true,
		"xhr.open('POST', p)": false, "$open(u)": false} {
		if got := chatSinkRE.MatchString(src); got != bad {
			t.Errorf("%q: matched %v, want %v", src, got, bad)
		}
	}
}

func TestChatPageHasNoMarkupSink(t *testing.T) {
	for _, name := range []string{"chatui/chat.js", "chatui/chatcore.js", "chatui/sw.js"} {
		b, err := chatUI.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if m := chatSinkRE.Find(b); m != nil {
			t.Errorf("%s contains %q: the chat page writes network text as text only", name, m)
		}
	}
	// What the script may build and where it may reach, counted: every
	// element it makes is a div, li, ul, button, span or a (a file row's
	// download link), the attributes it sets are the href of the two fixed
	// links and of a file row (lever's own download route, built from a
	// checked agent name and id) and that row's download name, the one
	// fetch is the api helper's, and the one XMLHttpRequest is the upload. A new element kind, attribute or request shows up here,
	// to be looked at.
	js, err := chatUI.ReadFile("chatui/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	for re, want := range map[string]int{
		`createElement\(`:            18,
		`createElement\('div'\)`:     8,
		`createElement\('li'\)`:      4,
		`createElement\('a'\)`:       1,
		`createElement\('button'\)`:  3,
		`createElement\('ul'\)`:      1,
		`createElement\('span'\)`:    1,
		`setAttribute\(`:             3,
		`setAttribute\('href', `:     2,
		`setAttribute\('download', `: 1,
		`new XMLHttpRequest\(`:       1,
		`\.open\(`:                   1,
		`\bfetch\(`:                  1,
		`serviceWorker\.register\('/lever/sw\.js', \{ scope: '/lever/' \}\)`: 1,
		`serviceWorker\.register\(`: 1,
		`pushManager\.subscribe\(`:  1,
		`new EventSource\(`:         1,
		`location\.reload\(\)`:      0,
	} {
		if got := len(regexp.MustCompile(re).FindAll(js, -1)); got != want {
			t.Errorf("chat.js has %d of %s, want %d: review what the new one writes or requests", got, re, want)
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

// TestServiceWorkerIsPushOnly: the worker has a push and a click handler
// and nothing that could sit between the page and its requests.
func TestServiceWorkerIsPushOnly(t *testing.T) {
	js, err := chatUI.ReadFile("chatui/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	for re, want := range map[string]int{
		`addEventListener\(`:                    2,
		`addEventListener\('push'`:              1,
		`addEventListener\('notificationclick'`: 1,
		`addEventListener\(\s*['"]fetch`:        0,
		`\bcaches\b`:                            0,
		`importScripts`:                         0,
		`skipWaiting|clients\.claim`:            0,
		`\bfetch\(`:                             1,
		`fetch\('/lever/api/agents'`:            1,
		`openWindow\(`:                          1,
	} {
		if got := len(regexp.MustCompile(re).FindAll(js, -1)); got != want {
			t.Errorf("sw.js has %d of %s, want %d", got, re, want)
		}
	}
}

// TestChatPageJS runs the page's own tests (chatui/*.test.mjs: its logic, and
// its script against a scripted hub in a fake browser) when node is installed.
func TestChatPageJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; chatui/*.test.mjs not run")
	}
	cmd := exec.Command(node, "--test")
	cmd.Dir = "chatui"
	cmd.Env = append(os.Environ(), "NODE_NO_WARNINGS=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}

// TestChatPageEmbedsOnlyThePage: the test support files beside the page are
// not in the binary.
func TestChatPageEmbedsOnlyThePage(t *testing.T) {
	entries, err := chatUI.ReadDir("chatui")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := "apple-touch-icon.png,chat.css,chat.html,chat.js,chatcore.js,icon-192.png,icon-512.png,icon-maskable-512.png,sw.js"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("embedded %s, want only the page's files: %s", got, want)
	}
}

// TestChatManifest: the page is installable as an app. The manifest names
// the instance, opens the page in its own window, and lists icons the proxy
// serves at the sizes it says.
func TestChatManifest(t *testing.T) {
	hub := newPageHub(t)
	h := NewHandler(chatConfig(t, hub))
	rw := chatDo(h, chatOp, "GET", chatManifestPath)
	if rw.Code != http.StatusOK || rw.Header().Get("Content-Type") != "application/manifest+json" {
		t.Fatalf("%d %q, want 200 application/manifest+json", rw.Code, rw.Header().Get("Content-Type"))
	}
	var m chatManifest
	if err := json.Unmarshal(rw.Body.Bytes(), &m); err != nil {
		t.Fatalf("manifest is not JSON: %v\n%s", err, rw.Body)
	}
	if m.Name != "boss · lever" || m.ShortName != "boss" || m.ID != "/lever/chat" || m.StartURL != "/lever/chat" ||
		m.Scope != "/lever/" || m.Display != "standalone" || m.BackgroundColor != "#f6f6f4" || m.ThemeColor != "#ffffff" {
		t.Errorf("manifest %+v", m)
	}
	// The colours are the page's own (chat.css, light scheme).
	css, err := chatUI.ReadFile("chatui/chat.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"--bg: " + m.BackgroundColor + ";", "--panel: " + m.ThemeColor + ";"} {
		if !strings.Contains(string(css), token) {
			t.Errorf("chat.css has no %q", token)
		}
	}
	var got []string
	for _, ic := range m.Icons {
		got = append(got, ic.Src+" "+ic.Sizes+" "+ic.Type+" "+ic.Purpose)
	}
	if want := "/lever/icon-192.png 192x192 image/png any,/lever/icon-512.png 512x512 image/png any," +
		"/lever/icon-maskable-512.png 512x512 image/png maskable"; strings.Join(got, ",") != want {
		t.Errorf("icons %v, want %s", got, want)
	}
	icons := map[string]int{"/lever/apple-touch-icon.png": 180}
	for _, ic := range m.Icons {
		var n int
		if _, err := fmt.Sscanf(ic.Sizes, "%dx", &n); err != nil {
			t.Fatal(err)
		}
		icons[ic.Src] = n
	}
	for src, size := range icons {
		rw := chatDo(h, chatOp, "GET", src)
		cfg, err := png.DecodeConfig(rw.Body)
		if rw.Code != http.StatusOK || err != nil || cfg.Width != size || cfg.Height != size {
			t.Errorf("%s: %d, %dx%d (%v), want a %dx%d PNG", src, rw.Code, cfg.Width, cfg.Height, err, size, size)
		}
	}
	// The page links the manifest and the Apple icon at the routes served.
	// The manifest link sends credentials: a front that authenticates by
	// cookie would otherwise turn the fetch away and the page could not
	// install.
	page := chatDo(h, chatOp, "GET", chatPagePath).Body.String()
	for _, link := range []string{`<link rel="manifest" href="` + chatManifestPath + `" crossorigin="use-credentials">`, `<link rel="apple-touch-icon" href="/lever/apple-touch-icon.png">`} {
		if !strings.Contains(page, link) {
			t.Errorf("chat.html lacks %s", link)
		}
	}
	if got := hub.reached(); len(got) != 0 {
		t.Fatalf("the hub was asked for the app's files: %v", got)
	}
}

// TestChatManifestShortName: a launcher shows about twelve characters.
func TestChatManifestShortName(t *testing.T) {
	for in, want := range map[string]string{"assistant": "assistant", "a-very-long-instance-name": "a-very-long-", "ééééééééééééé": "éééééééééééé"} {
		if got := chatManifestFor(in).ShortName; got != want {
			t.Errorf("short name for %q = %q, want %q", in, got, want)
		}
	}
}

// TestChatPageForContacts: with landing chat, a contact lands on the page,
// gets its files and its list, and never the hub's shell or console.
func TestChatPageForContacts(t *testing.T) {
	hub := newPageHub(t)
	var lines []AuditLine
	cfg := chatConfig(t, hub)
	cfg.Audit = func(l AuditLine) { lines = append(lines, l) }
	h := NewHandler(cfg)
	for _, p := range []string{"/", "/agents", "/chat/dm/" + url.PathEscape("dm:agent:"+agentW1+":user:u-c"), "/settings", "/agents/" + chatMgrID + "/terminal"} {
		for _, m := range []string{"GET", "HEAD"} {
			rw := chatDo(h, "c@x", m, p)
			if rw.Code != http.StatusFound || rw.Header().Get("Location") != chatPagePath || rw.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("%s %s as a contact: %d %q, want 302 %s", m, p, rw.Code, rw.Header().Get("Location"), chatPagePath)
			}
		}
	}
	for _, p := range []string{chatPagePath, "/lever/chat.js", "/lever/chatcore.js", "/lever/chat.css", "/lever/icon-192.png", chatManifestPath, chatAgentsPath} {
		if rw := chatDo(h, "c@x", "GET", p); rw.Code != http.StatusOK {
			t.Errorf("GET %s as a contact: %d", p, rw.Code)
		}
	}
	for p, code := range map[string]int{"/lever/api/chat": 404, "/lever/nope": 404} {
		if rw := chatDo(h, "c@x", "GET", p); rw.Code != code {
			t.Errorf("GET %s: %d, want %d", p, rw.Code, code)
		}
	}
	for _, p := range []string{chatPagePath, chatAgentsPath} {
		if rw := chatDo(h, "c@x", "POST", p, "Origin", "https://"+testServeHost); rw.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d, want 405", p, rw.Code)
		}
	}
	// Only the list reaches the hub, and only for the contact's identity and
	// DM read state, with its own session.
	for _, r := range hub.reached() {
		if r != "GET /api/v1/chat/dms" {
			t.Errorf("the hub saw a page request: %s", r)
		}
	}
	for _, l := range lines {
		if l.TSLogin != "c@x" || (l.Decision != DecisionAllow) {
			t.Errorf("audit %+v", l)
		}
	}
}

func TestContactManifestIsGeneric(t *testing.T) {
	hub := newPageHub(t)
	h := NewHandler(chatConfig(t, hub))
	c := chatDo(h, "c@x", "GET", chatManifestPath)
	o := chatDo(h, chatOp, "GET", chatManifestPath)
	if strings.Contains(c.Body.String(), "boss") || !strings.Contains(o.Body.String(), "boss") {
		t.Fatalf("contact manifest %s / operator manifest %s", c.Body, o.Body)
	}
	var m chatManifest
	if err := json.Unmarshal(c.Body.Bytes(), &m); err != nil || m.Name != "Chat · lever" || m.ShortName != "Chat" || m.StartURL != chatPagePath {
		t.Fatalf("contact manifest %+v %v", m, err)
	}
	if c.Header().Get("ETag") == o.Header().Get("ETag") {
		t.Fatal("two manifests share an ETag")
	}
}

// The fence's hub rules do not move: a contact's request for a hub route
// it may not use is still refused, landing chat or not.
func TestContactFenceUnchangedUnderChat(t *testing.T) {
	hub := newPageHub(t)
	hub.me = func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"id":"u-c"}`) }
	h := NewHandler(chatConfig(t, hub))
	for _, p := range []string{"/api/v1/agents/" + chatMgrID, "/api/v1/chat/conversations/" + url.PathEscape("dm:agent:id-w2:user:u-c") + "/messages",
		"/api/v1/chat/conversations/" + url.PathEscape("dm:agent:"+chatMgrID+":user:u-c") + "/messages", "/api/v1/projects/x"} {
		if rw := chatDo(h, "c@x", "GET", p); rw.Code != http.StatusForbidden {
			t.Errorf("GET %s: %d, want 403", p, rw.Code)
		}
	}
	// Its own conversation still works through the fence.
	own := "/api/v1/chat/conversations/" + url.PathEscape("dm:agent:"+agentW1+":user:u-c") + "/messages"
	if rw := chatDo(h, "c@x", "GET", own); rw.Code != http.StatusOK {
		t.Errorf("GET %s: %d, want 200", own, rw.Code)
	}
	if rw := chatDo(h, "c@x", "GET", "/assets/app.js"); rw.Code != http.StatusOK {
		t.Errorf("a static asset: %d", rw.Code)
	}
}

// landing unset: a contact still gets the old landing page.
func TestContactLandingWithoutChat(t *testing.T) {
	hub := newPageHub(t)
	cfg := chatConfig(t, hub)
	cfg.ChatAgent = ""
	h := NewHandler(cfg)
	for _, p := range []string{"/", chatPagePath, "/agents"} {
		if rw := chatDo(h, "c@x", "GET", p); rw.Code != 200 || !strings.Contains(rw.Body.String(), "<h1>Chat</h1>") {
			t.Fatalf("%s: %d %s", p, rw.Code, rw.Body.String())
		}
	}
	if rw := chatDo(h, "c@x", "GET", chatAgentsPath); rw.Code == http.StatusOK && strings.Contains(rw.Body.String(), "agents") {
		t.Fatalf("the agent list answered with the page off: %s", rw.Body)
	}
}
