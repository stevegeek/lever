package remoteproxy

import (
	"context"
	"encoding/json"
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
	mu     sync.Mutex
	calls  []bool // force, per call
	err    error
	backup string
	block  chan struct{}
}

func (n *newConvRec) fn(_ context.Context, force bool) (string, error) {
	n.mu.Lock()
	n.calls = append(n.calls, force)
	n.mu.Unlock()
	if n.block != nil {
		<-n.block
	}
	return n.backup, n.err
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
	return newConvPostTo(h, newConvPath, login, body, hdr...)
}

func newConvPostTo(h http.Handler, path, login, body string, hdr ...string) *httptest.ResponseRecorder {
	req := proxyRequest("POST", path, strings.NewReader(body))
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
	if rw.Code != http.StatusAccepted || newConvID(t, rw) == "" {
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

// newConvID reads the id of a 202 answer: state "starting" and nothing else.
func newConvID(t *testing.T, rw *httptest.ResponseRecorder) string {
	t.Helper()
	var ans map[string]string
	if err := json.Unmarshal(rw.Body.Bytes(), &ans); err != nil || ans["state"] != "starting" || len(ans) != 2 || len(ans["id"]) != 32 {
		t.Fatalf("answer %s (%v)", rw.Body, err)
	}
	return ans["id"]
}

func newConvStatus(h http.Handler, login, id string, hdr ...string) *httptest.ResponseRecorder {
	return newConvPostTo(h, newConvStatusPath, login, `{"id":"`+id+`"}`, hdr...)
}

// waitNewConvStatus reads the status until it is not running.
func waitNewConvStatus(t *testing.T, h http.Handler, id string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rw := newConvStatus(h, chatOp, id)
		body := strings.TrimSpace(rw.Body.String())
		if rw.Code != http.StatusOK || body != `{"state":"running"}` {
			return fmt.Sprintf("%d %s", rw.Code, body)
		}
		if time.Now().After(deadline) {
			t.Fatal("still running")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestNewConversationStatus: the change is followed by its own result, with
// the backup on success and a fixed word on failure; one change at a time;
// the status is the operator's, from the page only.
func TestNewConversationStatus(t *testing.T) {
	hub := newPageHub(t)
	n := &newConvRec{block: make(chan struct{}), backup: "20261019T054104Z"}
	var lines lockedLines
	cfg := newConvConfig(t, hub, n, "")
	cfg.Audit = lines.add
	g := NewHandler(cfg).(*gate)
	g.newConvWaitFor = 10 * time.Millisecond
	id := newConvID(t, newConvPost(g, chatOp, ""))
	if rw := newConvStatus(g, chatOp, id); rw.Code != http.StatusOK || strings.TrimSpace(rw.Body.String()) != `{"state":"running"}` {
		t.Fatalf("running: %d %s", rw.Code, rw.Body)
	}
	if all := lines.all(); all[len(all)-1].Decision != DecisionNewConversationStatus || all[len(all)-1].Reason != "running" {
		t.Fatalf("status audit %+v", all[len(all)-1])
	}
	// A second change while one runs is refused without spending the minute.
	g.newConvs.give(cfg.ChatAgent)
	if rw := newConvPost(g, chatOp, ""); rw.Code != http.StatusConflict || !strings.Contains(rw.Body.String(), `"in-progress"`) {
		t.Fatalf("second while running: %d %s", rw.Code, rw.Body)
	}
	if ok, _ := g.newConvs.take(cfg.ChatAgent); !ok {
		t.Fatal("the refused second spent the minute")
	}
	g.newConvs.give(cfg.ChatAgent)
	for name, rw := range map[string]*httptest.ResponseRecorder{
		"contact":      newConvStatus(g, "c@x", id),
		"no origin":    newConvStatus(g, chatOp, id, "Origin", ""),
		"cross origin": newConvStatus(g, chatOp, id, "Origin", "https://evil.test"),
		"cross site":   newConvStatus(g, chatOp, id, "Sec-Fetch-Site", "cross-site"),
	} {
		if rw.Code != http.StatusForbidden || strings.Contains(rw.Body.String(), "state") {
			t.Errorf("%s: %d %s", name, rw.Code, rw.Body)
		}
	}
	if rw := chatDo(g, chatOp, "GET", newConvStatusPath, "Origin", "https://"+testServeHost); rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rw.Code)
	}
	for _, body := range []string{"", "{", `{"id":7}`} {
		if rw := newConvPostTo(g, newConvStatusPath, chatOp, body); rw.Code != http.StatusBadRequest {
			t.Errorf("body %q: %d", body, rw.Code)
		}
	}
	for _, other := range []string{"", strings.Repeat("0", 32), id[:31]} {
		if rw := newConvStatus(g, chatOp, other); rw.Code != http.StatusNotFound || strings.TrimSpace(rw.Body.String()) != `{"error":"unknown"}` {
			t.Errorf("id %q: %d %s", other, rw.Code, rw.Body)
		}
	}
	close(n.block)
	if got := waitNewConvStatus(t, g, id); got != `200 {"backup":"20261019T054104Z","state":"ok"}` {
		t.Fatalf("ok: %s", got)
	}

	// No backup (an empty home), and a failure: fixed words only.
	for _, tc := range []struct {
		backup string
		err    error
		want   string
	}{
		{"", nil, `200 {"state":"ok"}`},
		{"20261019T054104Z", errors.New("secret detail"), `200 {"error":"failed","state":"failed"}`},
		{"", fmt.Errorf("wait: %w", context.DeadlineExceeded), `200 {"error":"timeout","state":"failed"}`},
		{"", ErrConversationLocked, `200 {"error":"in-progress","state":"failed"}`},
	} {
		n := &newConvRec{block: make(chan struct{}), backup: tc.backup, err: tc.err}
		g := NewHandler(newConvConfig(t, hub, n, "")).(*gate)
		g.newConvWaitFor = 10 * time.Millisecond
		id := newConvID(t, newConvPost(g, chatOp, ""))
		close(n.block)
		if got := waitNewConvStatus(t, g, id); got != tc.want {
			t.Errorf("%v: %s", tc.err, got)
		}
	}

	// A refusal within the answer's wait is the POST's answer, and the
	// status says the same.
	n = &newConvRec{err: ErrConversationBusy}
	g = NewHandler(newConvConfig(t, hub, n, "")).(*gate)
	if rw := newConvPost(g, chatOp, ""); rw.Code != http.StatusConflict || strings.Contains(rw.Body.String(), "id") {
		t.Fatalf("busy: %d %s", rw.Code, rw.Body)
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
