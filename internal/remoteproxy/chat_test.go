package remoteproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stevegeek/lever/internal/chatledger"
)

const (
	chatAgentID = "11111111-2222-3333-4444-555555555555"
	chatUserID  = "99999999-8888-7777-6666-555555555555"
	chatDMPath  = "/api/v1/chat/conversations/dm:agent:" + chatAgentID + ":user:" + chatUserID + "/messages"
	chatAnswer  = `{"id":"msg-1","content":"deploy the fix","sender":"user:op@example.com","senderId":"` + chatUserID +
		`","type":"instruction","createdAt":"2026-09-28T10:15:02.123456789Z","dispatchState":"dispatched"}`
)

// chatHub answers every request with status and body.
func chatHub(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

type ledgerSpy struct {
	mu      sync.Mutex
	entries []chatledger.Entry
}

func (l *ledgerSpy) append(e chatledger.Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
	return nil
}

func (l *ledgerSpy) all() []chatledger.Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]chatledger.Entry(nil), l.entries...)
}

// chatPost sends one request through a proxy in front of hub and returns the
// client's answer.
func chatPost(t *testing.T, hub *httptest.Server, allowed []string, login, method, path string, spy *ledgerSpy) *httptest.ResponseRecorder {
	t.Helper()
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(), ServeHost: testServeHost,
		AllowedUsers: allowed, ChatLedger: spy.append})
	req := proxyRequest(method, path, strings.NewReader(`{"content":"deploy the fix"}`))
	if login != "" {
		req.Header.Set("Tailscale-User-Login", login)
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	return rw
}

// TestChatPostIsRecordedForAVerifiedLogin: a DM post the hub accepted is
// recorded with the verified login, the DM's agent, the hub's id, sender and
// time (to the second, as the agent's envelope shows it), and the text — and
// the client still gets the hub's answer byte for byte.
func TestChatPostIsRecordedForAVerifiedLogin(t *testing.T) {
	spy := &ledgerSpy{}
	rw := chatPost(t, chatHub(t, http.StatusCreated, chatAnswer), []string{"op@example.com"}, "op@example.com", "POST", chatDMPath, spy)
	if rw.Code != http.StatusCreated || rw.Body.String() != chatAnswer {
		t.Fatalf("client got %d %q, want the hub's answer unchanged", rw.Code, rw.Body.String())
	}
	got := spy.all()
	if len(got) != 1 {
		t.Fatalf("recorded %d entries, want 1", len(got))
	}
	e := got[0]
	want := chatledger.Entry{Login: "op@example.com", Tier: chatledger.TierOperator,
		Conversation: "dm:agent:" + chatAgentID + ":user:" + chatUserID, AgentID: chatAgentID,
		MessageID: "msg-1", Sender: "user:op@example.com", CreatedAt: "2026-09-28T10:15:02Z", Text: "deploy the fix"}
	e.Recorded = want.Recorded
	if e != want {
		t.Fatalf("entry = %+v\nwant    %+v", e, want)
	}
}

// TestChatPostsThatAreNotRecorded: only a 201 answer to an agent-DM send from
// a verified login is recorded. Everything else stays unverifiable.
func TestChatPostsThatAreNotRecorded(t *testing.T) {
	allowed := []string{"op@example.com"}
	cases := []struct {
		name    string
		status  int
		body    string
		allowed []string
		method  string
		path    string
	}{
		{"no allowed_users, so no verified login", 201, chatAnswer, nil, "POST", chatDMPath},
		{"hub refused the post", 403, `{"error":"forbidden"}`, allowed, "POST", chatDMPath},
		{"idempotency replay (200)", 200, chatAnswer, allowed, "POST", chatDMPath},
		{"history read", 201, chatAnswer, allowed, "GET", chatDMPath},
		{"topic thread", 201, chatAnswer, allowed, "POST", "/api/v1/chat/conversations/topic-123/messages"},
		{"user-to-user DM", 201, chatAnswer, allowed, "POST", "/api/v1/chat/conversations/dm:user:a:user:b/messages"},
		{"edit", 201, chatAnswer, allowed, "PUT", chatDMPath + "/msg-1"},
		{"quick-message route", 201, chatAnswer, allowed, "POST", "/api/v1/agents/" + chatAgentID + "/message"},
		{"answer without an id", 201, `{"content":"x","sender":"user:op@example.com","createdAt":"2026-09-28T10:15:02Z"}`, allowed, "POST", chatDMPath},
		{"answer that is not JSON", 201, `<html>`, allowed, "POST", chatDMPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := &ledgerSpy{}
			rw := chatPost(t, chatHub(t, tc.status, tc.body), tc.allowed, "op@example.com", tc.method, tc.path, spy)
			if rw.Code != tc.status || rw.Body.String() != tc.body {
				t.Fatalf("client got %d %q, want the hub's %d %q", rw.Code, rw.Body.String(), tc.status, tc.body)
			}
			if n := len(spy.all()); n != 0 {
				t.Fatalf("recorded %d entries, want none", n)
			}
		})
	}
}

// TestChatDMAgent pins the route parser: only the exact agent-DM send route.
func TestChatDMAgent(t *testing.T) {
	if key, id, ok := chatDMAgent("POST", chatDMPath); !ok || id != chatAgentID || !strings.HasPrefix(key, "dm:agent:") {
		t.Fatalf("chatDMAgent(DM send) = %q, %q, %v", key, id, ok)
	}
	for _, p := range []string{
		"/api/v1/chat/conversations/dm:agent::user:u/messages",
		"/api/v1/chat/conversations/dm:agent:a:user:/messages",
		"/api/v1/chat/conversations/dm:agent:a:user:u:x/messages",
		"/api/v1/chat/conversations/dm:agent:a:user:u/messages/extra",
		"/api/v1/chat/conversations/dm:agent:a:user:u/read",
		"/api/v2/chat/conversations/dm:agent:a:user:u/messages",
	} {
		if _, _, ok := chatDMAgent("POST", p); ok {
			t.Errorf("chatDMAgent(%q) matched", p)
		}
	}
}

// TestALedgerFailureNeverFailsTheRequest: the message is already delivered
// when the hub answers, so a ledger that cannot write only loses the record.
func TestALedgerFailureNeverFailsTheRequest(t *testing.T) {
	hub := chatHub(t, http.StatusCreated, chatAnswer)
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(), ServeHost: testServeHost,
		AllowedUsers: []string{"op@example.com"},
		ChatLedger:   func(chatledger.Entry) error { return io.ErrShortWrite }})
	req := proxyRequest("POST", chatDMPath, strings.NewReader(`{}`))
	req.Header.Set("Tailscale-User-Login", "op@example.com")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusCreated || rw.Body.String() != chatAnswer {
		t.Fatalf("client got %d %q", rw.Code, rw.Body.String())
	}
}

// TestChatPostWithAnEncodedKeyIsRecorded: the SPA sends the conversation key
// through encodeURIComponent, so the colons arrive as %3A.
func TestChatPostWithAnEncodedKeyIsRecorded(t *testing.T) {
	spy := &ledgerSpy{}
	encoded := strings.ReplaceAll(chatDMPath, ":", "%3A")
	rw := chatPost(t, chatHub(t, http.StatusCreated, chatAnswer), []string{"op@example.com"}, "op@example.com", "POST", encoded, spy)
	if rw.Code != http.StatusCreated {
		t.Fatalf("status %d", rw.Code)
	}
	if got := spy.all(); len(got) != 1 || got[0].AgentID != chatAgentID {
		t.Fatalf("entries %+v, want one for %s", got, chatAgentID)
	}
}

// TestAPIResponsesAreSandboxed: an agent-written HTML file the hub serves
// inline under /api/ must not run on the proxy's origin with the operator's
// session (it could post chat that verifies as the operator). Every /api/
// answer carries a CSP sandbox; the SPA shell does not.
func TestAPIResponsesAreSandboxed(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Security-Policy", "script-src 'self' 'unsafe-inline'")
		_, _ = io.WriteString(w, "<script>fetch('/api/v1/auth/me')</script>")
	}))
	t.Cleanup(hub.Close)
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(), ServeHost: testServeHost})
	for path, want := range map[string]bool{
		"/api/v1/projects/p/workspace/files/report.html": true,
		"/api/v1/projects/p/dav/report.svg":              true,
		"/":                                              false,
		"/assets/app.js":                                 false,
	} {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, proxyRequest("GET", path, nil))
		got := false
		for _, v := range rw.Header().Values("Content-Security-Policy") {
			if v == "sandbox" {
				got = true
			}
		}
		if got != want {
			t.Errorf("%s: sandbox CSP = %v, want %v (headers %v)", path, got, want, rw.Header())
		}
		if want && rw.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", path)
		}
	}
}

// TestASandboxedDocumentCannotPost: a request from a sandboxed document
// carries Origin: null, which the gate refuses before the hub sees it.
func TestASandboxedDocumentCannotPost(t *testing.T) {
	spy := &ledgerSpy{}
	hub := chatHub(t, http.StatusCreated, chatAnswer)
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(), ServeHost: testServeHost,
		AllowedUsers: []string{"op@example.com"}, ChatLedger: spy.append})
	req := proxyRequest("POST", chatDMPath, strings.NewReader(`{"content":"x"}`))
	req.Header.Set("Tailscale-User-Login", "op@example.com")
	req.Header.Set("Origin", "null")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusForbidden || len(spy.all()) != 0 {
		t.Fatalf("status %d, entries %d; want 403 and nothing recorded", rw.Code, len(spy.all()))
	}
}
