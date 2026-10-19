package broker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/scion"
)

func postOperatorResume(t *testing.T, b *Broker, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, PathOperatorWorkerResume, strings.NewReader(body))
	rec := httptest.NewRecorder()
	b.OperatorHandler().ServeHTTP(rec, req)
	return rec
}

// `lever worker resume` runs the manager's resume verb as the operator: a
// fresh ticket, the resume (forced for an error-phase record), the wait, and
// an audit line naming the operator.
func TestOperatorResumeResumesARecord(t *testing.T) {
	for _, tc := range []struct {
		phase         string
		plain, forced int
	}{
		{"suspended", 1, 0},
		{"stopped", 1, 0},
		{"completed", 1, 0},
		{"error", 0, 1},
	} {
		rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: {{Slug: "worker", Phase: tc.phase}}}}
		b := newTestBroker(t, rt, wakeSpec)
		var buf bytes.Buffer
		b.log = slog.New(slog.NewTextHandler(&buf, nil))
		rec := postOperatorResume(t, b, `{"worker":"worker"}`)
		if rec.Code != http.StatusOK || len(rt.resumed) != tc.plain || len(rt.resumeForced) != tc.forced {
			t.Fatalf("%s: %d %s resumed=%d forced=%d", tc.phase, rec.Code, rec.Body, len(rt.resumed), len(rt.resumeForced))
		}
		if bs := rt.lastStaged(t, "worker"); bs.AgentCN != "worker" {
			t.Fatalf("%s: no fresh ticket staged", tc.phase)
		}
		if !strings.Contains(buf.String(), "caller=operator") || !strings.Contains(buf.String(), "decision=allow") {
			t.Fatalf("%s: audit must name the operator:\n%s", tc.phase, buf.String())
		}
		if rec.Header().Get("Deprecation") != "" {
			t.Fatalf("%s: the new route is not deprecated", tc.phase)
		}
	}
}

// A running worker is a no-op success: nothing staged, nothing resumed.
func TestOperatorResumeOfARunningWorkerIsANoOp(t *testing.T) {
	rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: {{Slug: "worker", Phase: "running"}}}}
	b := newTestBroker(t, rt, wakeSpec)
	rec := postOperatorResume(t, b, `{"worker":"worker"}`)
	if rec.Code != http.StatusOK || len(rt.resumed) != 0 || len(rt.staged) != 0 {
		t.Fatalf("%d resumed=%d staged=%d", rec.Code, len(rt.resumed), len(rt.staged))
	}
	if !strings.Contains(rec.Body.String(), `"phase":"running"`) {
		t.Fatalf("body %s", rec.Body)
	}
}

// Every refusal carries its documented reason, and changes nothing.
func TestOperatorResumeRefusalReasons(t *testing.T) {
	asleep := []scion.Agent{{Slug: "worker", Phase: "suspended"}}
	for name, tc := range map[string]struct {
		agents []scion.Agent
		body   string
		setup  func(*Broker, *fakeRuntime)
		code   int
		reason string
	}{
		"bad body":  {asleep, `{`, nil, http.StatusBadRequest, ReasonBadRequest},
		"unknown":   {asleep, `{"worker":"zz"}`, nil, http.StatusForbidden, ReasonNotAWorker},
		"manager":   {asleep, `{"worker":"test-manager"}`, nil, http.StatusForbidden, ReasonNotAWorker},
		"no record": {nil, `{"worker":"worker"}`, nil, http.StatusNotFound, ReasonNoRecord},
		"revoked":   {asleep, `{"worker":"worker"}`, func(b *Broker, _ *fakeRuntime) { b.Revoke("worker") }, http.StatusForbidden, ReasonRevoked},
		"role": {asleep, `{"worker":"worker"}`, func(b *Broker, _ *fakeRuntime) {
			b.verifyRole = func(context.Context, string) error { return errors.New("stored role reads as full") }
		}, http.StatusConflict, ReasonRoleMismatch},
		"list fails":   {asleep, `{"worker":"worker"}`, func(_ *Broker, rt *fakeRuntime) { rt.listErr = errors.New("hub down") }, http.StatusBadGateway, ReasonRuntimeError},
		"no runtime":   {asleep, `{"worker":"worker"}`, func(b *Broker, _ *fakeRuntime) { b.runtime = nil }, http.StatusBadGateway, ReasonRuntimeUnavailable},
		"hub refuses":  {asleep, `{"worker":"worker"}`, func(_ *Broker, rt *fakeRuntime) { rt.resumeErr = errHubRefusedForTest }, http.StatusConflict, ReasonHubRefused},
		"resume fails": {asleep, `{"worker":"worker"}`, func(_ *Broker, rt *fakeRuntime) { rt.resumeErr = errors.New("podman: env SECRET=x") }, http.StatusBadGateway, ReasonRuntimeError},
	} {
		rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: tc.agents}}
		b := newTestBroker(t, rt, wakeSpec)
		if tc.setup != nil {
			tc.setup(b, rt)
		}
		rec := postOperatorResume(t, b, tc.body)
		if rec.Code != tc.code || rec.Header().Get(ReasonHeader) != tc.reason {
			t.Errorf("%s: %d %q (want %d %q): %s", name, rec.Code, rec.Header().Get(ReasonHeader), tc.code, tc.reason, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "SECRET") {
			t.Errorf("%s: runtime error text leaked: %s", name, rec.Body)
		}
		if tc.reason != ReasonHubRefused && tc.reason != ReasonRuntimeError && (len(rt.resumed) != 0 || len(rt.staged) != 0) {
			t.Errorf("%s: a refusal must change nothing: resumed=%d staged=%d", name, len(rt.resumed), len(rt.staged))
		}
	}
}

// The hub's refusal names the phase it refused, as a known label.
func TestOperatorResumeHubRefusalNamesThePhase(t *testing.T) {
	rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: {{Slug: "worker", Phase: "stopped"}}}, resumeErr: errHubRefusedForTest}
	b := newTestBroker(t, rt, wakeSpec)
	rec := postOperatorResume(t, b, `{"worker":"worker"}`)
	if rec.Header().Get(PhaseHeader) != "stopped" {
		t.Fatalf("phase header %q", rec.Header().Get(PhaseHeader))
	}
}

// Only the operator socket serves the route: never the jail's mTLS listener
// or the admin loopback.
func TestOperatorResumeIsNotOnTheTCPListeners(t *testing.T) {
	b := newTestBroker(t, &fakeRuntime{}, wakeSpec)
	for name, h := range map[string]http.Handler{"admin": b.AdminHandler(), "jail": b.JailHandler()} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, PathOperatorWorkerResume, strings.NewReader(`{"worker":"worker"}`)))
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s handler answers %s: %d", name, PathOperatorWorkerResume, rec.Code)
		}
	}
}

// The remote chat page's resume answers with reasons and phases too.
func TestOperatorResumeForReasons(t *testing.T) {
	for _, tc := range []struct {
		agents       []scion.Agent
		body         string
		reason, phse string
	}{
		{[]scion.Agent{{Slug: "worker", Phase: "stopped"}}, `{"worker":"worker","login":"c@x","tier":"contact"}`, ReasonStoppedByOperator, "stopped"},
		{[]scion.Agent{{Slug: "worker", Phase: "error"}}, `{"worker":"worker","login":"c@x"}`, ReasonNotAsleep, "error"},
		{nil, `{"worker":"worker","login":"c@x"}`, ReasonNoRecord, ""},
		{nil, `{"worker":"zz","login":"c@x"}`, ReasonNotAWorker, ""},
	} {
		rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: tc.agents}}
		rec := postWake(t, newTestBroker(t, rt, wakeSpec), tc.body)
		if rec.Header().Get(ReasonHeader) != tc.reason || rec.Header().Get(PhaseHeader) != tc.phse {
			t.Errorf("%s: reason %q phase %q (want %q %q)", tc.body, rec.Header().Get(ReasonHeader), rec.Header().Get(PhaseHeader), tc.reason, tc.phse)
		}
	}
}

// A note to a worker that is not running is refused with a reason and its
// phase (#239), and the text names the host command.
func TestOperatorNoteRefusalReason(t *testing.T) {
	for _, tc := range []struct {
		agents        []scion.Agent
		reason, phase string
		text          string
	}{
		{[]scion.Agent{{Slug: "worker", Phase: "suspended"}}, ReasonNotRunning, "suspended", "lever worker resume worker"},
		{nil, ReasonNoRecord, "", "has no record"},
	} {
		rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: tc.agents}}
		b := newTestBroker(t, rt, wakeSpec)
		rec := postNote(t, b, `{"to":"worker","body":"x"}`)
		if rec.Code != http.StatusConflict || rec.Header().Get(ReasonHeader) != tc.reason || rec.Header().Get(PhaseHeader) != tc.phase {
			t.Errorf("%v: %d reason %q phase %q", tc.agents, rec.Code, rec.Header().Get(ReasonHeader), rec.Header().Get(PhaseHeader))
		}
		if !strings.Contains(rec.Body.String(), tc.text) {
			t.Errorf("%v: body %q lacks %q", tc.agents, rec.Body, tc.text)
		}
	}
}

// errHubRefusedForTest reads as a hub refusal to scion.IsRefusedByHub.
var errHubRefusedForTest = errors.New("resume: missing required scope agent:lifecycle")

// A revoke that lands while the resume waits for the worker's lock is seen
// under the lock: nothing is staged or resumed.
func TestOperatorResumeRechecksRevocationUnderTheLock(t *testing.T) {
	rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: {{Slug: "worker", Phase: "suspended"}}}}
	b := newTestBroker(t, rt, wakeSpec)
	unlock, err := b.lockWorker(context.Background(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	b.onWorkerLockWait = func(string) {
		b.Revoke("worker")
		unlock()
	}
	rec := postOperatorResume(t, b, `{"worker":"worker"}`)
	if rec.Code != http.StatusForbidden || rec.Header().Get(ReasonHeader) != ReasonRevoked || len(rt.resumed) != 0 || len(rt.staged) != 0 {
		t.Fatalf("%d %q resumed=%d staged=%d", rec.Code, rec.Header().Get(ReasonHeader), len(rt.resumed), len(rt.staged))
	}
}

// A record the hub calls running while its container is down goes out as
// one token in PhaseHeader.
func TestRefusePhaseTokenizesContainerDown(t *testing.T) {
	rec := httptest.NewRecorder()
	refusePhase(rec, ReasonNotRunning, phaseRunningContainerDown, "x", http.StatusConflict)
	if got := rec.Header().Get(PhaseHeader); got != PhaseRunningContainerDownToken {
		t.Fatalf("phase header %q", got)
	}
}

// The operator's refusals name what the operator can run; the manager's
// text is unchanged.
func TestResumeHubRefusalHintByActor(t *testing.T) {
	asleep := map[string][]scion.Agent{testInstanceProject: {{Slug: "worker", Phase: "stopped"}}}
	rt := &fakeRuntime{agents: asleep, resumeErr: errHubRefusedForTest}
	b := newTestBroker(t, rt, wakeSpec)
	if body := postOperatorResume(t, b, `{"worker":"worker"}`).Body.String(); strings.Contains(body, "lever-manager") || !strings.Contains(body, "lever doctor") {
		t.Fatalf("operator text: %s", body)
	}
	rt2 := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: {{Slug: "worker", Phase: "stopped"}}}, resumeErr: errHubRefusedForTest}
	b2 := newTestBroker(t, rt2, wakeSpec)
	if body := callWorker(t, b2, "/worker/resume", `{"worker":"worker"}`, "test-manager").Body.String(); !strings.Contains(body, "check `lever-manager agent list`") {
		t.Fatalf("manager text changed: %s", body)
	}
}
