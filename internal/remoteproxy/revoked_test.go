package remoteproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

// revocation is a LoginCurrent whose answer a test changes while the
// handler runs, as `lever apply` rewrites the file the real one reads.
type revocation struct {
	mu      sync.Mutex
	revoked map[string]bool
}

func (r *revocation) revoke(login string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.revoked == nil {
		r.revoked = map[string]bool{}
	}
	r.revoked[login] = true
}

func (r *revocation) current(login string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.revoked[login] {
		return errors.New("revoked")
	}
	return nil
}

// A login revoked after the proxy started is refused everywhere the proxy
// answers from its cached session (chat page, wake, files) and on every hub
// route, while the others keep working.
func TestRevokedLoginIsRefusedOnEveryRoute(t *testing.T) {
	hub := newPageHub(t)
	wr := &wakeRec{}
	rev := &revocation{}
	var lines lockedLines
	cfg := wakeConfig(t, hub, wr)
	cfg.LoginCurrent = rev.current
	cfg.Audit = lines.add
	h := NewHandler(cfg)
	if rw := chatDo(h, "c@x", "GET", chatPagePath); rw.Code != http.StatusOK {
		t.Fatalf("before the removal: chat page %d", rw.Code)
	}
	rev.revoke("c@x")
	before := len(hub.reached())
	for _, c := range []struct{ method, path string }{
		{"GET", "/"}, {"GET", chatPagePath}, {"GET", chatAgentsPath}, {"GET", "/api/v1/auth/me"}, {"GET", "/lever/api/files/w1"},
		{"GET", "/auth/login"}, {"GET", "/events?sub=x"},
	} {
		if rw := chatDo(h, "c@x", c.method, c.path); rw.Code != http.StatusForbidden {
			t.Errorf("%s %s: %d, want 403", c.method, c.path, rw.Code)
		}
	}
	if rw := wakePost(h, "c@x", "w1"); rw.Code != http.StatusForbidden {
		t.Errorf("wake: %d, want 403", rw.Code)
	}
	if len(wr.got()) != 0 {
		t.Fatalf("a revoked login reached the broker: %v", wr.got())
	}
	if n := len(hub.reached()); n != before {
		t.Fatalf("a revoked login reached the hub: %v", hub.reached()[before:])
	}
	if !slices.ContainsFunc(lines.all(), func(l AuditLine) bool { return l.Decision == DecisionDenyRevoked && l.TSLogin == "c@x" }) {
		t.Fatalf("no %s audit line: %+v", DecisionDenyRevoked, lines.all())
	}
	if rw := chatDo(h, chatOp, "GET", chatPagePath); rw.Code != http.StatusOK {
		t.Fatalf("the operator, not revoked: %d", rw.Code)
	}
}

// Without allowed_users the proxy's one identity is "", and a config that
// now pins logins revokes it.
func TestRevokedUnnamedOperator(t *testing.T) {
	hub := newRecordingHub(t)
	rev := &revocation{}
	rev.revoke("")
	h := NewHandler(Config{Target: mustURL(t, hub.URL), Session: testSession(), ServeHost: "mac.ts.net", LoginCurrent: rev.current})
	rw := chatDo(h, "", "GET", "/api/v1/agents")
	if rw.Code != http.StatusForbidden || hub.hits() != 0 {
		t.Fatalf("status %d, hub hits %d", rw.Code, hub.hits())
	}
}

func TestRevokedLoginGetsNoPush(t *testing.T) {
	e := newTriggerEnv(t, nil)
	rev := &revocation{}
	e.g.cfg.LoginCurrent = rev.current
	e.p.store.Add(chatOp, sub("a"))
	rev.revoke(chatOp)
	e.p.notify(context.Background(), chatOp, "w1")
	if n := len(e.fs.all()); n != 0 {
		t.Fatalf("%d pushes sent to a revoked login", n)
	}
	if err := e.p.streamOnce(context.Background(), chatOp); !errors.Is(err, errPushSession) {
		t.Fatalf("streamOnce for a revoked login: %v", err)
	}
}

// An /events stream opened before the revocation ends with it: no new
// request would reach the check.
func TestRevocationEndsAnOpenStream(t *testing.T) {
	hub := newPageHub(t)
	hub.route = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/events" {
			return false
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for {
			if _, err := io.WriteString(w, "data: {}\n\n"); err != nil {
				return true
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return true
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	rev := &revocation{}
	cfg := chatConfig(t, hub)
	cfg.LoginCurrent = rev.current
	h := NewHandler(cfg).(*gate)
	h.revokeEvery = 20 * time.Millisecond
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest("GET", srv.URL+"/events?sub=x", nil)
	req.Host = testServeHost
	req.Header.Set("Tailscale-User-Login", chatOp)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("the stream did not open: %v", err)
	}
	rev.revoke(chatOp)
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, resp.Body); done <- err }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the open stream outlived the revocation")
	}
}
