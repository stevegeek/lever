package remoteproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type wakeRec struct {
	mu    sync.Mutex
	calls []string
	tiers []string
	err   error
	block chan struct{}
}

func (w *wakeRec) fn(ctx context.Context, login, tier, worker string) error {
	w.mu.Lock()
	w.calls = append(w.calls, login+" "+worker)
	w.tiers = append(w.tiers, tier)
	w.mu.Unlock()
	if w.block != nil {
		<-w.block
	}
	return w.err
}

func (w *wakeRec) got() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.calls)
}

func wakeConfig(t *testing.T, hub *pageHub, wr *wakeRec) Config {
	cfg := chatConfig(t, hub) // boss manager; workers w1 w2 w3; contact c@x agents [w1], see [w2]
	cfg.AgentRecords = func(context.Context) (map[string]AgentRecord, error) {
		return map[string]AgentRecord{"boss": {ID: chatMgrID, Phase: "suspended"}, "w1": {ID: agentW1, Phase: "suspended"},
			"w2": {ID: "id-w2", Phase: "suspended"}, "w3": {ID: "id-w3", Phase: "stopped"}}, nil
	}
	cfg.Wake = wr.fn
	return cfg
}

func wakePost(h http.Handler, login, name string, hdr ...string) *httptest.ResponseRecorder {
	return chatDo(h, login, "POST", "/lever/api/agents/"+name+"/wake",
		append([]string{"Origin", "https://" + testServeHost, "Sec-Fetch-Site", "same-origin"}, hdr...)...)
}

// lockedLines collects audit lines from more than one goroutine.
type lockedLines struct {
	mu    sync.Mutex
	lines []AuditLine
}

func (l *lockedLines) add(a AuditLine) { l.mu.Lock(); l.lines = append(l.lines, a); l.mu.Unlock() }
func (l *lockedLines) all() []AuditLine {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

func TestWakeASuspendedWorker(t *testing.T) {
	hub := newPageHub(t)
	wr := &wakeRec{}
	var lines lockedLines
	cfg := wakeConfig(t, hub, wr)
	cfg.Audit = lines.add
	rw := wakePost(NewHandler(cfg), "c@x", "w1")
	if rw.Code != http.StatusAccepted || strings.TrimSpace(rw.Body.String()) != `{"state":"starting"}` {
		t.Fatalf("%d %s", rw.Code, rw.Body.String())
	}
	for k, v := range map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store", "Content-Security-Policy": "sandbox"} {
		if got := rw.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if !slices.Equal(wr.got(), []string{"c@x w1"}) || !slices.Equal(wr.tiers, []string{"contact"}) {
		t.Fatalf("broker calls %v tiers %v", wr.got(), wr.tiers)
	}
	all := lines.all()
	last := all[len(all)-1]
	if last.Decision != DecisionWake || last.Status != http.StatusAccepted || last.TSLogin != "c@x" || last.Path != "/lever/api/agents/w1/wake" {
		t.Fatalf("audit %+v", all)
	}
	// The operator wakes a stopped worker the same way.
	if rw := wakePost(NewHandler(cfg), chatOp, "w3"); rw.Code != http.StatusAccepted {
		t.Fatalf("operator: %d", rw.Code)
	}
}

// Review Focus 2.
func TestWakeRefusesWhatTheLoginMayNotMessage(t *testing.T) {
	hub := newPageHub(t)
	wr := &wakeRec{}
	var lines lockedLines
	cfg := wakeConfig(t, hub, wr)
	cfg.Audit = lines.add
	h := NewHandler(cfg)
	var first string
	for _, tc := range []struct{ login, name string }{
		{"c@x", "boss"}, {"c@x", "w2"}, {"c@x", "w3"}, {"c@x", "nope"}, {chatOp, "boss"}, {chatOp, "nope"},
	} {
		rw := wakePost(h, tc.login, tc.name)
		if rw.Code != http.StatusForbidden {
			t.Errorf("%s wakes %s: %d", tc.login, tc.name, rw.Code)
		}
		if first == "" {
			first = rw.Body.String()
		} else if rw.Body.String() != first {
			t.Errorf("%s wakes %s: body %q differs from %q (tells hidden from absent)", tc.login, tc.name, rw.Body.String(), first)
		}
	}
	if strings.TrimSpace(first) != `{"error":"not-allowed"}` {
		t.Errorf("body %q", first)
	}
	for _, l := range lines.all() {
		if l.Decision != DecisionDenyWake || l.Reason != "not-allowed" {
			t.Errorf("audit %+v", l)
		}
	}
	if len(wr.got()) != 0 {
		t.Fatalf("broker called: %v", wr.got())
	}
	// The limiter was not touched: the allowed wake still passes at once.
	if rw := wakePost(h, "c@x", "w1"); rw.Code != http.StatusAccepted {
		t.Fatalf("after refusals: %d", rw.Code)
	}
}

// Review Focus 3.
func TestWakeOriginRules(t *testing.T) {
	hub := newPageHub(t)
	wr := &wakeRec{}
	h := NewHandler(wakeConfig(t, hub, wr))
	for name, hdr := range map[string][]string{
		"no origin":    {"Origin", ""},
		"cross origin": {"Origin", "https://evil.test"},
		"null origin":  {"Origin", "null"},
		"cross site":   {"Sec-Fetch-Site", "cross-site"},
		"typed (none)": {"Sec-Fetch-Site", "none"},
		"same-site":    {"Sec-Fetch-Site", "same-site"},
	} {
		req := proxyRequest("POST", "/lever/api/agents/w1/wake", nil)
		req.Header.Set("Tailscale-User-Login", "c@x")
		req.Header.Set("Origin", "https://"+testServeHost)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		if hdr[1] == "" {
			req.Header.Del(hdr[0])
		} else {
			req.Header.Set(hdr[0], hdr[1])
		}
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, rw.Code)
		}
	}
	// Two Origin headers are not one origin.
	req := proxyRequest("POST", "/lever/api/agents/w1/wake", nil)
	req.Header.Set("Tailscale-User-Login", "c@x")
	req.Header.Add("Origin", "https://"+testServeHost)
	req.Header.Add("Origin", "https://"+testServeHost)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Errorf("two origins: %d", rw.Code)
	}
	for _, m := range []string{"GET", "HEAD", "PUT", "DELETE"} {
		if rw := chatDo(h, "c@x", m, "/lever/api/agents/w1/wake", "Origin", "https://"+testServeHost); rw.Code != http.StatusMethodNotAllowed || rw.Header().Get("Allow") != "POST" {
			t.Errorf("%s wake: %d Allow %q", m, rw.Code, rw.Header().Get("Allow"))
		}
	}
	if len(wr.got()) != 0 {
		t.Fatalf("broker called: %v", wr.got())
	}
	// None of that spent the agent's wake.
	if rw := wakePost(h, "c@x", "w1"); rw.Code != http.StatusAccepted {
		t.Fatalf("after refusals: %d", rw.Code)
	}
}

func TestWakeRateLimitIsPerAgentAcrossLogins(t *testing.T) {
	hub := newPageHub(t)
	wr := &wakeRec{}
	cfg := wakeConfig(t, hub, wr)
	now := time.Unix(1000, 0)
	h := NewHandler(cfg).(*gate)
	h.wakes.now = func() time.Time { return now }
	if rw := wakePost(h, "c@x", "w1"); rw.Code != http.StatusAccepted {
		t.Fatalf("first: %d", rw.Code)
	}
	now = now.Add(20 * time.Second)
	rw := wakePost(h, chatOp, "w1")
	if rw.Code != http.StatusTooManyRequests || rw.Header().Get("Retry-After") != "40" || !strings.Contains(rw.Body.String(), `"rate-limited"`) {
		t.Fatalf("second: %d Retry-After %q %s", rw.Code, rw.Header().Get("Retry-After"), rw.Body)
	}
	now = now.Add(500 * time.Millisecond)
	if rw := wakePost(h, chatOp, "w1"); rw.Header().Get("Retry-After") != "40" {
		t.Fatalf("Retry-After rounds up: %q", rw.Header().Get("Retry-After"))
	}
	if rw := wakePost(h, chatOp, "w3"); rw.Code != http.StatusAccepted {
		t.Fatalf("another worker: %d", rw.Code)
	}
	now = now.Add(40 * time.Second)
	if rw := wakePost(h, chatOp, "w1"); rw.Code != http.StatusAccepted {
		t.Fatalf("after 60 s: %d", rw.Code)
	}
	if got := wr.got(); !slices.Equal(got, []string{"c@x w1", chatOp + " w3", chatOp + " w1"}) {
		t.Fatalf("broker calls %v", got)
	}
}

func TestWakeStateAndBrokerAnswers(t *testing.T) {
	recs := func(phase string) func(context.Context) (map[string]AgentRecord, error) {
		return func(context.Context) (map[string]AgentRecord, error) {
			if phase == "" {
				return map[string]AgentRecord{}, nil
			}
			return map[string]AgentRecord{"w1": {ID: agentW1, Phase: phase}}, nil
		}
	}
	for name, tc := range map[string]struct {
		login   string
		mut     func(*Config, *wakeRec)
		code    int
		word    string
		state   string
		brokers int
	}{
		"running":          {"c@x", func(c *Config, _ *wakeRec) { c.AgentRecords = recs("running") }, 409, "not-asleep", "running", 0},
		"starting":         {"c@x", func(c *Config, _ *wakeRec) { c.AgentRecords = recs("resumed") }, 409, "not-asleep", "starting", 0},
		"contact stopped":  {"c@x", func(c *Config, _ *wakeRec) { c.AgentRecords = recs("stopped") }, 409, "not-asleep", "stopped", 0},
		"operator stopped": {chatOp, func(c *Config, _ *wakeRec) { c.AgentRecords = recs("stopped") }, 202, "", "starting", 1},
		"error":            {chatOp, func(c *Config, _ *wakeRec) { c.AgentRecords = recs("error") }, 409, "not-asleep", "error", 0},
		"no record":        {chatOp, func(c *Config, _ *wakeRec) { c.AgentRecords = recs("") }, 409, "not-asleep", "no-record", 0},
		"hub down":         {chatOp, func(c *Config, _ *wakeRec) { c.AgentRecords = nil }, 409, "not-asleep", "unknown", 0},
		"not fresh":        {"c@x", func(c *Config, _ *wakeRec) { c.ContactSession = func(string) error { return errors.New("old") } }, 409, "not-asleep", "not-fresh", 0},
		"op not fresh":     {chatOp, func(c *Config, _ *wakeRec) { c.ContactSession = func(string) error { return errors.New("old") } }, 202, "", "starting", 1},
		"no wake":          {"c@x", func(c *Config, _ *wakeRec) { c.Wake = nil }, 503, "unavailable", "", 0},
		"broker 409":       {"c@x", func(_ *Config, w *wakeRec) { w.err = &WakeError{Status: 409, Err: errors.New("not asleep")} }, 409, "refused", "", 1},
		"broker 403":       {"c@x", func(_ *Config, w *wakeRec) { w.err = &WakeError{Status: 403, Err: errors.New("x")} }, 409, "refused", "", 1},
		"broker 502":       {"c@x", func(_ *Config, w *wakeRec) { w.err = &WakeError{Status: 502, Err: errors.New("x")} }, 502, "failed", "", 1},
		"broker absent":    {"c@x", func(_ *Config, w *wakeRec) { w.err = &WakeError{Err: errors.New("dial")} }, 502, "failed", "", 1},
		"plain error":      {"c@x", func(_ *Config, w *wakeRec) { w.err = errors.New("x") }, 502, "failed", "", 1},
	} {
		t.Run(name, func(t *testing.T) {
			hub := newPageHub(t)
			wr := &wakeRec{}
			cfg := wakeConfig(t, hub, wr)
			tc.mut(&cfg, wr)
			rw := wakePost(NewHandler(cfg), tc.login, "w1")
			var body map[string]string
			_ = json.Unmarshal(rw.Body.Bytes(), &body)
			if rw.Code != tc.code || body["error"] != tc.word || body["state"] != tc.state || len(wr.got()) != tc.brokers {
				t.Fatalf("%d %s calls=%d, want %d %q state %q calls=%d", rw.Code, rw.Body, len(wr.got()), tc.code, tc.word, tc.state, tc.brokers)
			}
			if strings.Contains(rw.Body.String(), "not asleep") || strings.Contains(rw.Body.String(), "dial") {
				t.Fatalf("the broker's text leaked: %s", rw.Body)
			}
		})
	}
}

// A broker that takes longer than the answer wait: the page gets 202 at
// once, and the late result is one more audit line, with no error text.
func TestWakeAnswersBeforeTheResumeEnds(t *testing.T) {
	hub := newPageHub(t)
	wr := &wakeRec{block: make(chan struct{}), err: &WakeError{Status: 502, Err: errors.New("secret detail")}}
	var lines lockedLines
	cfg := wakeConfig(t, hub, wr)
	cfg.Audit = lines.add
	g := NewHandler(cfg).(*gate)
	g.wakeWaitFor = 10 * time.Millisecond
	if rw := wakePost(g, "c@x", "w1"); rw.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	close(wr.block)
	deadline := time.Now().Add(5 * time.Second)
	for {
		all := lines.all()
		if l := all[len(all)-1]; l.Decision == DecisionWakeResult {
			if l.Status != http.StatusBadGateway || l.Reason != "failed" || strings.Contains(l.Error, "secret") || l.Path != "/lever/api/agents/w1/wake" {
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

// Only a plain agent name routes to the wake; anything else under the
// agents prefix is a 404, and never reaches the broker.
func TestWakeRouteShape(t *testing.T) {
	hub := newPageHub(t)
	wr := &wakeRec{}
	h := NewHandler(wakeConfig(t, hub, wr))
	for _, p := range []string{"/lever/api/agents/w1%2Fx/wake", "/lever/api/agents/W1/wake", "/lever/api/agents//wake",
		"/lever/api/agents/w1/wake/", "/lever/api/agents/w1/wakeup", "/lever/api/agents/-w1/wake", "/lever/api/agents/w1/x/wake",
		"/lever/api/agents/" + strings.Repeat("a", 64) + "/wake"} {
		req := proxyRequest("POST", p, nil)
		req.Header.Set("Tailscale-User-Login", chatOp)
		req.Header.Set("Origin", "https://"+testServeHost)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusNotFound && rw.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d", p, rw.Code)
		}
	}
	if len(wr.got()) != 0 || len(hub.reached()) != 0 {
		t.Fatalf("broker %v hub %v", wr.got(), hub.reached())
	}
}
