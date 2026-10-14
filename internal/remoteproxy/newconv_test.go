package remoteproxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type newConvRec struct {
	mu    sync.Mutex
	calls []bool // force, per call
	err   error
	block chan struct{}
}

func (n *newConvRec) fn(_ context.Context, force bool) error {
	n.mu.Lock()
	n.calls = append(n.calls, force)
	n.mu.Unlock()
	if n.block != nil {
		<-n.block
	}
	return n.err
}

func (n *newConvRec) got() []bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.calls)
}

func newConvConfig(t *testing.T, hub *pageHub, n *newConvRec, activity string) Config {
	cfg := chatConfig(t, hub) // boss manager (running); contact c@x
	cfg.AgentRecords = func(context.Context) (map[string]AgentRecord, error) {
		return map[string]AgentRecord{"boss": {ID: chatMgrID, Phase: "running", Activity: activity}}, nil
	}
	cfg.NewConversation = n.fn
	return cfg
}

func newConvPost(h http.Handler, login, body string, hdr ...string) *httptest.ResponseRecorder {
	req := proxyRequest("POST", newConvPath, strings.NewReader(body))
	req.Header.Set("Tailscale-User-Login", login)
	req.Header.Set("Origin", "https://"+testServeHost)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			req.Header.Del(hdr[i])
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	return rw
}

// TestNewConversationOperatorOnly: the operator's login starts it; a contact,
// a cross-site or originless POST, and every other method never reach lever.
func TestNewConversationOperatorOnly(t *testing.T) {
	hub := newPageHub(t)
	n := &newConvRec{}
	var lines lockedLines
	cfg := newConvConfig(t, hub, n, "completed")
	cfg.Audit = lines.add
	h := NewHandler(cfg)
	if rw := newConvPost(h, "c@x", ""); rw.Code != http.StatusForbidden || strings.TrimSpace(rw.Body.String()) != `{"error":"not-allowed"}` {
		t.Fatalf("contact: %d %s", rw.Code, rw.Body)
	}
	for name, hdr := range map[string][]string{
		"no origin":    {"Origin", ""},
		"cross origin": {"Origin", "https://evil.test"},
		"cross site":   {"Sec-Fetch-Site", "cross-site"},
	} {
		if rw := newConvPost(h, chatOp, "", hdr...); rw.Code != http.StatusForbidden {
			t.Errorf("%s: %d", name, rw.Code)
		}
	}
	for _, m := range []string{"GET", "HEAD", "PUT"} {
		if rw := chatDo(h, chatOp, m, newConvPath, "Origin", "https://"+testServeHost); rw.Code != http.StatusMethodNotAllowed || rw.Header().Get("Allow") != "POST" {
			t.Errorf("%s: %d", m, rw.Code)
		}
	}
	if len(n.got()) != 0 {
		t.Fatalf("lever called: %v", n.got())
	}
	// None of that spent the minute's change.
	rw := newConvPost(h, chatOp, "")
	if rw.Code != http.StatusAccepted || strings.TrimSpace(rw.Body.String()) != `{"state":"starting"}` {
		t.Fatalf("operator: %d %s", rw.Code, rw.Body)
	}
	if !slices.Equal(n.got(), []bool{false}) {
		t.Fatalf("calls %v", n.got())
	}
	all := lines.all()
	if l := all[len(all)-1]; l.Decision != DecisionNewConversation || l.TSLogin != chatOp || l.Path != newConvPath {
		t.Fatalf("audit %+v", l)
	}
	for _, l := range all[:len(all)-1] {
		if !strings.HasPrefix(string(l.Decision), "deny-") {
			t.Errorf("refusal audited as %+v", l)
		}
	}
	// One a minute.
	if rw := newConvPost(h, chatOp, ""); rw.Code != http.StatusTooManyRequests || rw.Header().Get("Retry-After") == "" {
		t.Fatalf("second: %d", rw.Code)
	}
}

// TestNewConversationOfferedToTheOperatorOnly: only the operator's list
// carries the offer, and only when the route is wired.
func TestNewConversationOfferedToTheOperatorOnly(t *testing.T) {
	hub := newPageHub(t)
	cfg := newConvConfig(t, hub, &newConvRec{}, "")
	if _, ans := agentsAs(t, NewHandler(cfg), chatOp); !ans.NewConversation {
		t.Fatal("operator: not offered")
	}
	if _, ans := agentsAs(t, NewHandler(cfg), "c@x"); ans.NewConversation {
		t.Fatal("contact: offered")
	}
	cfg.NewConversation = nil
	h := NewHandler(cfg)
	if _, ans := agentsAs(t, h, chatOp); ans.NewConversation {
		t.Fatal("offered while not wired")
	}
	if rw := newConvPost(h, chatOp, ""); rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("not wired: %d", rw.Code)
	}
}

// TestNewConversationBusyAndState: a turn in progress is refused unless
// forced; a manager that is not up is refused; lever's own refusals map to
// fixed words.
func TestNewConversationBusyAndState(t *testing.T) {
	hub := newPageHub(t)
	n := &newConvRec{}
	h := NewHandler(newConvConfig(t, hub, n, "working"))
	if rw := newConvPost(h, chatOp, ""); rw.Code != http.StatusConflict || strings.TrimSpace(rw.Body.String()) != `{"error":"busy"}` {
		t.Fatalf("busy: %d %s", rw.Code, rw.Body)
	}
	if rw := newConvPost(h, chatOp, `{"force":true}`); rw.Code != http.StatusAccepted {
		t.Fatalf("forced: %d %s", rw.Code, rw.Body)
	}
	if !slices.Equal(n.got(), []bool{true}) {
		t.Fatalf("calls %v", n.got())
	}
	for _, body := range []string{"{", `{"force":"yes"}`, strings.Repeat(" ", newConvBodyMax+1)} {
		if rw := newConvPost(NewHandler(newConvConfig(t, hub, n, "")), chatOp, body); rw.Code != http.StatusBadRequest {
			t.Errorf("body %.10q: %d", body, rw.Code)
		}
	}

	cfg := newConvConfig(t, hub, n, "")
	cfg.AgentRecords = func(context.Context) (map[string]AgentRecord, error) {
		return map[string]AgentRecord{"boss": {ID: chatMgrID, Phase: "stopped"}}, nil
	}
	if rw := newConvPost(NewHandler(cfg), chatOp, ""); rw.Code != http.StatusConflict || !strings.Contains(rw.Body.String(), `"not-running"`) {
		t.Fatalf("stopped: %d %s", rw.Code, rw.Body)
	}
	cfg.AgentRecords = func(context.Context) (map[string]AgentRecord, error) { return nil, errors.New("hub down") }
	if rw := newConvPost(NewHandler(cfg), chatOp, ""); rw.Code != http.StatusConflict || !strings.Contains(rw.Body.String(), `"unknown"`) {
		t.Fatalf("hub down: %d %s", rw.Code, rw.Body)
	}

	// lever's own busy changed nothing, so it does not spend the minute: the
	// forced retry the page then makes goes through.
	n = &newConvRec{err: ErrConversationBusy}
	h = NewHandler(newConvConfig(t, hub, n, ""))
	if rw := newConvPost(h, chatOp, ""); rw.Code != http.StatusConflict {
		t.Fatalf("lever busy: %d", rw.Code)
	}
	n.err = nil
	if rw := newConvPost(h, chatOp, `{"force":true}`); rw.Code != http.StatusAccepted {
		t.Fatalf("forced after lever's busy: %d", rw.Code)
	}

	for err, want := range map[error]string{
		ErrConversationBusy:                              "busy",
		ErrConversationLocked:                            "in-progress",
		errors.New("secret"):                             "failed",
		fmt.Errorf("wait: %w", context.DeadlineExceeded): "timeout",
	} {
		n := &newConvRec{err: err}
		rw := newConvPost(NewHandler(newConvConfig(t, hub, n, "")), chatOp, "")
		if rw.Code == http.StatusAccepted || !strings.Contains(rw.Body.String(), `"`+want+`"`) || strings.Contains(rw.Body.String(), "secret") {
			t.Errorf("%v: %d %s", err, rw.Code, rw.Body)
		}
	}
}

// TestNewConversationAnswersBeforeItEnds: the page is told it is starting;
// the outcome is one more audit line, without lever's text.
func TestNewConversationAnswersBeforeItEnds(t *testing.T) {
	hub := newPageHub(t)
	n := &newConvRec{block: make(chan struct{}), err: errors.New("secret detail")}
	var lines lockedLines
	cfg := newConvConfig(t, hub, n, "")
	cfg.Audit = lines.add
	g := NewHandler(cfg).(*gate)
	g.newConvWaitFor = 10 * time.Millisecond
	if rw := newConvPost(g, chatOp, ""); rw.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	close(n.block)
	deadline := time.Now().Add(5 * time.Second)
	for {
		all := lines.all()
		if l := all[len(all)-1]; l.Decision == DecisionNewConversationResult {
			if l.Status != http.StatusBadGateway || l.Reason != "failed" || strings.Contains(l.Error, "secret") {
				t.Fatalf("late line %+v", l)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no late result line: %+v", all)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
