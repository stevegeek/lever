package remoteproxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// unsafePathSpellings are paths that some reader could resolve to another
// route than the one the proxy's prefix checks see. The first ones are the
// contact-tier escape from an allowed static prefix (card #81).
var unsafePathSpellings = []string{
	"/assets/../lever/api/chat",
	"/assets/%2e%2e/lever/api/chat",
	"/assets/%2E%2E/lever/api/chat",
	"/assets/.%2e/lever/api/chat",
	"/assets/..%2flever/api/chat",
	"/assets/..%2Flever/api/chat",
	"/assets%2f..%2flever/api/chat",
	`/assets/..\lever`,
	"/assets/..%5clever",
	"/assets/..%5Clever",
	"/shoelace/../api/v1/agents",
	"/assets/./app.js",
	"/assets/.",
	"/assets/..",
	"//lever/api",
	"/assets//app.js",
	"//api/v1/auth/tokens",
	"/api/v1/auth/./cli",
	"/api%2fv1/auth/tokens",
	"/assets/app.js%00",
	"/chat/dm/x/../../api/v1/agents",
}

// TestUnsafePathRefusedForEveryTier: each spelling is refused with 400 before
// any route decision, for a contact and for the operator, and never reaches
// the hub.
func TestUnsafePathRefusedForEveryTier(t *testing.T) {
	hub := newContactHub(t)
	h := contactHandler(t, hub, nil)
	for _, login := range []string{"c@x", "op@x"} {
		for _, p := range unsafePathSpellings {
			for _, m := range []string{http.MethodGet, http.MethodPost} {
				rw := contactDo(h, login, m, p, "")
				if rw.Code != http.StatusBadRequest {
					t.Errorf("%s %s %s: %d %s, want 400", login, m, p, rw.Code, rw.Body)
				}
			}
		}
	}
	if got := hub.reached(); len(got) != 0 {
		t.Errorf("hub reached by %q", got)
	}
}

// TestSafePathsStillPass: the spellings the web UI uses, and ones that only
// look like the refused class, are forwarded as before.
func TestSafePathsStillPass(t *testing.T) {
	hub := newContactHub(t)
	h := contactHandler(t, hub, nil)
	contact := []string{
		"/assets/app.js",
		"/assets/index-B1x.2.js",
		"/assets/..app.js",
		"/assets/app..js",
		"/shoelace/assets/icons/x.svg",
		"/favicon.svg",
		"/api/v1/auth/me",
		dmPath(agentW1, contactUID, "/messages"),
		"/chat/dm/" + url.PathEscape("dm:agent:"+agentW1+":user:"+contactUID),
	}
	for _, p := range contact {
		if rw := contactDo(h, "c@x", http.MethodGet, p, ""); rw.Code != http.StatusOK {
			t.Errorf("contact GET %s: %d %s, want forwarded", p, rw.Code, rw.Body)
		}
	}
	operator := []string{"/", "/assets/app.js", "/api/v1/agents", "/api/v1/auth/%74okens", "/api/v1/projects/p1/agents"}
	for _, p := range operator {
		if rw := contactDo(h, "op@x", http.MethodGet, p, ""); rw.Code != http.StatusOK {
			t.Errorf("operator GET %s: %d %s, want forwarded", p, rw.Code, rw.Body)
		}
	}
}

func TestUnsafePathAuditDecision(t *testing.T) {
	var lines []AuditLine
	hub := newContactHub(t)
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(), ServeHost: testServeHost,
		Audit: func(l AuditLine) { lines = append(lines, l) }})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, proxyRequest(http.MethodGet, "/assets/%2e%2e/api/v1/agents", nil))
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rw.Code)
	}
	if len(lines) != 1 || lines[0].Decision != DecisionDenyPath || lines[0].Status != http.StatusBadRequest {
		t.Fatalf("audit %+v, want one %s/400 line", lines, DecisionDenyPath)
	}
}
