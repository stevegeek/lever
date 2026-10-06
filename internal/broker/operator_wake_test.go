package broker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

func postWake(t *testing.T, b *Broker, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, wire.PathOperatorWake, strings.NewReader(body))
	rec := httptest.NewRecorder()
	b.OperatorHandler().ServeHTTP(rec, req)
	return rec
}

var wakeSpec = WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", TicketDir: "/run/user/501/lever/tickets/worker"}

// The remote chat page's wake runs the resume verb's path: a fresh ticket,
// the resume, the wait, and an audit line that names the remote login.
func TestOperatorWakeResumesASuspendedWorker(t *testing.T) {
	for _, phase := range []string{"suspended", "stopped"} {
		rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: {{Slug: "worker", Phase: phase}}}}
		b := newTestBroker(t, rt, wakeSpec)
		var buf bytes.Buffer
		b.log = slog.New(slog.NewTextHandler(&buf, nil))
		rec := postWake(t, b, `{"worker":"worker","login":"c@x"}`)
		if rec.Code != http.StatusOK || len(rt.resumed) != 1 {
			t.Fatalf("%s: %d %s resumed=%d", phase, rec.Code, rec.Body.String(), len(rt.resumed))
		}
		if bs := rt.lastStaged(t, "worker"); bs.AgentCN != "worker" {
			t.Fatalf("%s: no fresh ticket staged", phase)
		}
		if !strings.Contains(buf.String(), "remote:c@x") || !strings.Contains(buf.String(), "decision=allow") {
			t.Fatalf("%s: audit must name the remote login:\n%s", phase, buf.String())
		}
	}
}

func TestOperatorWakeRefuses(t *testing.T) {
	asleep := []scion.Agent{{Slug: "worker", Phase: "suspended"}}
	for name, tc := range map[string]struct {
		agents []scion.Agent
		body   string
		code   int
	}{
		"manager":   {asleep, `{"worker":"test-manager","login":"c@x"}`, http.StatusForbidden},
		"unknown":   {asleep, `{"worker":"zz","login":"c@x"}`, http.StatusForbidden},
		"empty":     {asleep, `{"login":"c@x"}`, http.StatusForbidden},
		"error":     {[]scion.Agent{{Slug: "worker", Phase: "error"}}, `{"worker":"worker","login":"c@x"}`, http.StatusConflict},
		"starting":  {[]scion.Agent{{Slug: "worker", Phase: "starting"}}, `{"worker":"worker","login":"c@x"}`, http.StatusConflict},
		"resumed":   {[]scion.Agent{{Slug: "worker", Phase: "resumed"}}, `{"worker":"worker","login":"c@x"}`, http.StatusConflict},
		"no record": {nil, `{"worker":"worker","login":"c@x"}`, http.StatusConflict},
		"bad body":  {nil, `{`, http.StatusBadRequest},
	} {
		rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: tc.agents}}
		b := newTestBroker(t, rt, wakeSpec)
		rec := postWake(t, b, tc.body)
		if rec.Code != tc.code || len(rt.resumed) != 0 || len(rt.staged) != 0 {
			t.Errorf("%s: %d (want %d) resumed=%d staged=%d", name, rec.Code, tc.code, len(rt.resumed), len(rt.staged))
		}
	}
	// The manager's scion slug is refused as well: only a declared worker wakes.
	rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: {{Slug: "appname", Phase: "suspended"}}}}
	b := New(testConfig(t, withManager("test-manager", "appname"), withRuntime(rt, wakeSpec)))
	if rec := postWake(t, b, `{"worker":"appname","login":"c@x"}`); rec.Code != http.StatusForbidden || len(rt.resumed) != 0 {
		t.Errorf("manager slug: %d resumed=%d", rec.Code, len(rt.resumed))
	}
}

// A role refusal is a refusal (409), with nothing staged or resumed.
func TestOperatorWakeRoleRefusal(t *testing.T) {
	rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: {{Slug: "worker", Phase: "suspended"}}}}
	b := newTestBroker(t, rt, wakeSpec)
	b.verifyRole = func(context.Context, string) error { return errors.New("stored role reads as full") }
	var buf bytes.Buffer
	b.log = slog.New(slog.NewTextHandler(&buf, nil))
	rec := postWake(t, b, `{"worker":"worker","login":"c@x"}`)
	if rec.Code != http.StatusConflict || len(rt.resumed) != 0 || len(rt.staged) != 0 {
		t.Fatalf("%d resumed=%d staged=%d", rec.Code, len(rt.resumed), len(rt.staged))
	}
	if !strings.Contains(buf.String(), "decision=deny") {
		t.Fatalf("role refusal must audit as deny:\n%s", buf.String())
	}
}

// A login is caller text: the audit keeps it to one bounded token.
func TestOperatorWakeBoundsTheLogin(t *testing.T) {
	if got := boundedLogin("a b\nc\x00" + strings.Repeat("x", 300)); len(got) != 120 || strings.ContainsAny(got, " \n\x00") {
		t.Fatalf("%q", got)
	}
}

// The wake route is the operator socket's alone: the admin listener is
// unauthenticated loopback that the hub's network can reach.
func TestOperatorWakeIsNotOnTheTCPListeners(t *testing.T) {
	b := newTestBroker(t, &fakeRuntime{}, wakeSpec)
	for name, h := range map[string]http.Handler{"admin": b.AdminHandler(), "jail": b.JailHandler()} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, wire.PathOperatorWake, strings.NewReader(`{"worker":"worker"}`)))
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s handler answers %s: %d", name, wire.PathOperatorWake, rec.Code)
		}
	}
}

// slowResumeRuntime is fakeRuntime made safe for concurrent calls, with a
// resume that takes until gate closes. While a resume is under way the
// hub still reports the record suspended, as the real one does.
type slowResumeRuntime struct {
	*fakeRuntime
	mu        sync.Mutex
	resuming  bool
	gate      chan struct{}
	entered   chan struct{}
	enterOnce sync.Once
}

func (s *slowResumeRuntime) List(ctx context.Context, project string) ([]scion.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resuming {
		return []scion.Agent{{Slug: "worker", Phase: "suspended"}}, nil
	}
	return s.fakeRuntime.List(ctx, project)
}

func (s *slowResumeRuntime) Resume(ctx context.Context, worker, project string) error {
	s.mu.Lock()
	s.resuming = true
	err := s.fakeRuntime.Resume(ctx, worker, project)
	s.mu.Unlock()
	s.enterOnce.Do(func() { close(s.entered) })
	<-s.gate
	s.mu.Lock()
	s.resuming = false
	s.mu.Unlock()
	return err
}

func (s *slowResumeRuntime) StageWorkerTicket(ctx context.Context, worker string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeRuntime.StageWorkerTicket(ctx, worker, payload)
}

// The manager's resume verb and a remote wake of the same worker at once:
// one resume, and neither caller fails. Without a per-worker lock both read
// "suspended" and both resume; the second resume fails on a live record.
func TestResumeAndWakeOfOneWorkerDoNotRace(t *testing.T) {
	rt := &slowResumeRuntime{fakeRuntime: &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: {{Slug: "worker", Phase: "suspended"}}}},
		gate: make(chan struct{}), entered: make(chan struct{})}
	b := newTestBroker(t, rt, wakeSpec)
	verb := make(chan int, 1)
	go func() { verb <- callWorker(t, b, "/worker/resume", `{"worker":"worker"}`, "test-manager").Code }()
	<-rt.entered
	wake := make(chan *httptest.ResponseRecorder, 1)
	go func() { wake <- postWake(t, b, `{"worker":"worker","login":"c@x"}`) }()
	time.Sleep(50 * time.Millisecond) // let the wake reach the lock (or, without one, the runtime)
	close(rt.gate)
	if code := <-verb; code != http.StatusOK {
		t.Fatalf("verb: %d", code)
	}
	if rec := <-wake; rec.Code != http.StatusOK {
		t.Fatalf("wake: %d %s", rec.Code, rec.Body)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.resumed) != 1 {
		t.Fatalf("resumed %d times, want once", len(rt.resumed))
	}
}

// A wake that finds the worker running (another resume won the lock) is a
// no-op success: the caller wanted it live, and it is.
func TestOperatorWakeOfARunningWorkerIsANoOp(t *testing.T) {
	rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: {{Slug: "worker", Phase: "running"}}}}
	b := newTestBroker(t, rt, wakeSpec)
	if rec := postWake(t, b, `{"worker":"worker","login":"c@x"}`); rec.Code != http.StatusOK || len(rt.resumed) != 0 || len(rt.staged) != 0 {
		t.Fatalf("%d resumed=%d staged=%d", rec.Code, len(rt.resumed), len(rt.staged))
	}
}
