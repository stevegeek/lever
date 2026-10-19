package broker

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/scion"
)

const stoppedDown = "Exited (0) 1 second ago"

// newSessionBroker is a broker whose worker reads phase first and stopped
// after the stop, with a session recorder that counts begins and commits.
func newSessionBroker(t *testing.T, phase, taskFile string) (*Broker, *fakeRuntime, *bytes.Buffer, *[2]int) {
	t.Helper()
	b, rt, audit := operatorStartBroker(t, phase, false, taskFile)
	b.liveInterval = time.Millisecond
	// The phase read, the image check before the stop, the stop wait, the
	// resume's image check, and the read just before the resume.
	rt.listFirst = [][]scion.Agent{
		{{Slug: "worker", Phase: phase, ContainerStatus: "Up 1 minute"}},
		{{Slug: "worker", Phase: phase, ContainerStatus: "Up 1 minute"}},
		{{Slug: "worker", Phase: scion.PhaseStopped, ContainerStatus: stoppedDown}},
		{{Slug: "worker", Phase: scion.PhaseStopped, ContainerStatus: stoppedDown}},
		{{Slug: "worker", Phase: scion.PhaseStopped, ContainerStatus: stoppedDown}},
	}
	var sessions [2]int
	b.beginSession = func(agent string) func() error {
		sessions[0]++
		return func() error { sessions[1]++; return nil }
	}
	return b, rt, audit, &sessions
}

// The worker is stopped, then its stopped record is resumed in a new session
// with the task: the request's, else the task_file, else the hub's stored
// one (a plain resume). The new session is recorded like a fresh start.
func TestOperatorWorkerNewSession(t *testing.T) {
	for _, tc := range []struct {
		body, file, wantTask string
	}{
		{`{"worker":"worker","task":"job 7"}`, "from the file", "job 7"},
		{`{"worker":"worker"}`, "from the file", "from the file"},
		{`{"worker":"worker"}`, "", ""},
	} {
		b, rt, _, sessions := newSessionBroker(t, scion.PhaseRunning, tc.file)
		rec := callOperator(t, b, http.MethodPost, PathOperatorWorkerNewSession, tc.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %q %s", tc.body, rec.Code, rec.Header().Get(ReasonHeader), rec.Body)
		}
		if len(rt.stopped) != 1 || len(rt.resumed) != 1 || len(rt.purged) != 0 || len(rt.started) != 0 {
			t.Fatalf("%s: stopped=%v resumed=%v purged=%v started=%d", tc.body, rt.stopped, rt.resumed, rt.purged, len(rt.started))
		}
		if tc.wantTask == "" && len(rt.resumeTasks) != 0 || tc.wantTask != "" && (len(rt.resumeTasks) != 1 || rt.resumeTasks[0] != tc.wantTask) {
			t.Fatalf("%s: resume tasks %q, want %q", tc.body, rt.resumeTasks, tc.wantTask)
		}
		if *sessions != [2]int{1, 1} {
			t.Fatalf("%s: session begins/commits = %v", tc.body, *sessions)
		}
		if len(rt.staged["worker"]) != 1 {
			t.Fatalf("%s: a fresh ticket must be staged once, got %d", tc.body, len(rt.staged["worker"]))
		}
	}
}

// A suspended record would take the task into the session it continues, so
// it is stopped first like any other.
func TestOperatorWorkerNewSessionStopsASuspendedWorker(t *testing.T) {
	b, rt, _, _ := newSessionBroker(t, scion.PhaseSuspended, "next job")
	rt.listFirst[0][0].ContainerStatus = stoppedDown
	if rec := callOperator(t, b, http.MethodPost, PathOperatorWorkerNewSession, `{"worker":"worker"}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(rt.stopped) != 1 || len(rt.resumeTasks) != 1 || rt.resumeTasks[0] != "next job" {
		t.Fatalf("stopped=%v tasks=%q", rt.stopped, rt.resumeTasks)
	}
}

// Every check that can refuse the resume runs before the stop: a refusal
// leaves the worker's session as it was.
func TestOperatorWorkerNewSessionRefusesBeforeTheStop(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		setup  func(*Broker, *fakeRuntime)
		code   int
		reason string
	}{
		"unknown":   {`{"worker":"zz"}`, nil, http.StatusForbidden, ReasonNotAWorker},
		"manager":   {`{"worker":"test-manager"}`, nil, http.StatusForbidden, ReasonNotAWorker},
		"revoked":   {`{"worker":"worker"}`, func(b *Broker, _ *fakeRuntime) { b.Revoke("worker") }, http.StatusForbidden, ReasonRevoked},
		"flag task": {`{"worker":"worker","task":"-x"}`, nil, http.StatusBadRequest, ReasonBadTask},
		"no record": {`{"worker":"worker"}`, func(_ *Broker, rt *fakeRuntime) { rt.listFirst = [][]scion.Agent{{}} }, http.StatusNotFound, ReasonNoRecord},
		"task_file": {`{"worker":"worker"}`, func(b *Broker, _ *fakeRuntime) { setTaskPath(b, "/nonexistent/task.md") }, http.StatusInternalServerError, ReasonConfigFile},
		"role": {`{"worker":"worker"}`, func(b *Broker, _ *fakeRuntime) {
			b.verifyRole = func(context.Context, string) error { return errors.New("stored role is not baseline") }
		}, http.StatusConflict, ReasonRoleMismatch},
		"workspace gone": {`{"worker":"worker"}`, func(b *Broker, _ *fakeRuntime) {
			b.readOnlyDirs, b.tree = []string{"protected"}, b.workers["worker"].HostWorkspace+"/../.."
			if err := os.RemoveAll(b.workers["worker"].HostWorkspace); err != nil {
				panic(err)
			}
		}, http.StatusConflict, ReasonWorkspaceMissing},
		"worker cap": {`{"worker":"worker"}`, func(b *Broker, rt *fakeRuntime) {
			b.maxRunningWorkers = 1
			b.workers["other"] = WorkerSpec{Name: "other"}
			rt.listFirst = [][]scion.Agent{
				{{Slug: "worker", Phase: "running", ContainerStatus: "Up 1 minute"}},
				{{Slug: "worker", Phase: "running", ContainerStatus: "Up 1 minute"}},
				{{Slug: "other", Phase: "running", ContainerStatus: "Up 1 minute"}},
			}
		}, http.StatusConflict, ReasonWorkerCap},
		"unqualified image": {`{"worker":"worker"}`, func(_ *Broker, rt *fakeRuntime) {
			rt.listFirst[1][0].Image = "scionlocal/lever-claude:1"
		}, http.StatusConflict, ReasonImageUnqualified},
		"no image": {`{"worker":"worker"}`, func(_ *Broker, rt *fakeRuntime) { rt.keepEmptyImage = true }, http.StatusConflict, ReasonImageUnqualified},
	} {
		b, rt, _, sessions := newSessionBroker(t, scion.PhaseRunning, "from the file")
		if tc.setup != nil {
			tc.setup(b, rt)
		}
		rec := callOperator(t, b, http.MethodPost, PathOperatorWorkerNewSession, tc.body)
		if rec.Code != tc.code || rec.Header().Get(ReasonHeader) != tc.reason {
			t.Errorf("%s: %d %q (want %d %q): %s", name, rec.Code, rec.Header().Get(ReasonHeader), tc.code, tc.reason, rec.Body)
		}
		if len(rt.stopped) != 0 || len(rt.resumed) != 0 || len(rt.staged["worker"]) != 0 || *sessions != [2]int{} {
			t.Errorf("%s: a refusal must change nothing: stopped=%v resumed=%v staged=%d sessions=%v", name, rt.stopped, rt.resumed, len(rt.staged["worker"]), *sessions)
		}
	}
}

// A stop that fails, or that neither the hub nor podman confirms, starts no
// new session.
func TestOperatorWorkerNewSessionNeedsAConfirmedStop(t *testing.T) {
	for name, setup := range map[string]func(*Broker, *fakeRuntime){
		"stop fails": func(_ *Broker, rt *fakeRuntime) { rt.stopErr = errors.New("scion: SECRET=1") },
		"container still running": func(b *Broker, _ *fakeRuntime) {
			b.containerRunning = func(context.Context, string) (bool, error) { return true, nil }
		},
		"hub still reads running": func(_ *Broker, rt *fakeRuntime) {
			rt.listFirst = [][]scion.Agent{{{Slug: "worker", Phase: "running", ContainerStatus: "Up 1 minute"}}}
			rt.staticPhases = true
			rt.agents[testInstanceProject] = []scion.Agent{{Slug: "worker", Phase: "stopped", ContainerStatus: "Up 1 minute"}}
		},
	} {
		b, rt, _, sessions := newSessionBroker(t, scion.PhaseRunning, "")
		setup(b, rt)
		rec := callOperator(t, b, http.MethodPost, PathOperatorWorkerNewSession, `{"worker":"worker"}`)
		if rec.Code != http.StatusBadGateway || rec.Header().Get(ReasonHeader) != ReasonRuntimeError || strings.Contains(rec.Body.String(), "SECRET") {
			t.Errorf("%s: %d %q %s", name, rec.Code, rec.Header().Get(ReasonHeader), rec.Body)
		}
		if len(rt.resumed) != 0 || *sessions != [2]int{} {
			t.Errorf("%s: resumed=%v sessions=%v", name, rt.resumed, *sessions)
		}
	}
}

// new-session takes the worker's lock and waits for another lifecycle verb
// to finish, so it never stops a worker another verb is bringing up.
func TestOperatorWorkerNewSessionWaitsForTheLock(t *testing.T) {
	b, rt, _, _ := newSessionBroker(t, scion.PhaseRunning, "")
	unlock, ok := b.tryLockWorker("worker")
	if !ok {
		t.Fatal("lock")
	}
	waiting := make(chan struct{})
	b.onWorkerLockWait = func(string) { close(waiting) }
	done := make(chan int)
	go func() {
		done <- callOperator(t, b, http.MethodPost, PathOperatorWorkerNewSession, `{"worker":"worker"}`).Code
	}()
	<-waiting
	if len(rt.stopped) != 0 {
		t.Fatal("stopped while another verb held the lock")
	}
	unlock()
	if code := <-done; code != http.StatusOK || len(rt.stopped) != 1 {
		t.Fatalf("%d stopped=%v", code, rt.stopped)
	}
}

// A revoke that lands while the worker stops leaves it stopped: no ticket,
// no resume.
func TestOperatorWorkerNewSessionRevokedDuringTheStop(t *testing.T) {
	b, rt, _, sessions := newSessionBroker(t, scion.PhaseRunning, "")
	b.containerRunning = func(context.Context, string) (bool, error) {
		b.Revoke("worker")
		return false, nil
	}
	rec := callOperator(t, b, http.MethodPost, PathOperatorWorkerNewSession, `{"worker":"worker"}`)
	if rec.Code != http.StatusForbidden || rec.Header().Get(ReasonHeader) != ReasonRevoked {
		t.Fatalf("%d %q %s", rec.Code, rec.Header().Get(ReasonHeader), rec.Body)
	}
	if len(rt.stopped) != 1 || len(rt.resumed) != 0 || len(rt.staged["worker"]) != 0 || *sessions != [2]int{} {
		t.Fatalf("stopped=%v resumed=%v staged=%d sessions=%v", rt.stopped, rt.resumed, len(rt.staged["worker"]), *sessions)
	}
}

// A record that turns suspended (a worker can post its own phase) between
// the stop wait and the resume is not resumed with the task.
func TestOperatorWorkerNewSessionRereadsThePhase(t *testing.T) {
	b, rt, _, sessions := newSessionBroker(t, scion.PhaseRunning, "next job")
	rt.listFirst[2] = []scion.Agent{{Slug: "worker", Phase: scion.PhaseSuspended, ContainerStatus: stoppedDown}}
	rec := callOperator(t, b, http.MethodPost, PathOperatorWorkerNewSession, `{"worker":"worker"}`)
	if rec.Code != http.StatusBadGateway || rec.Header().Get(ReasonHeader) != ReasonRuntimeError {
		t.Fatalf("%d %q %s", rec.Code, rec.Header().Get(ReasonHeader), rec.Body)
	}
	if len(rt.resumed) != 0 || len(rt.resumeTasks) != 0 || *sessions != [2]int{} {
		t.Fatalf("resumed=%v tasks=%q sessions=%v", rt.resumed, rt.resumeTasks, *sessions)
	}
}

// Every resume checks revocation once more before it stages a ticket:
// revoke takes no worker lock.
func TestResumeRecordRefusesARevokedWorker(t *testing.T) {
	b, rt, _, spec := recycleBroker(t, scion.PhaseSuspended, false)
	b.Revoke("worker")
	rec := httptest.NewRecorder()
	b.resumeRecordAs(context.Background(), rec, spec, scion.PhaseSuspended, operatorActor, nil)
	if rec.Code != http.StatusForbidden || len(rt.staged["worker"]) != 0 || len(rt.resumed) != 0 {
		t.Fatalf("%d staged=%d resumed=%v", rec.Code, len(rt.staged["worker"]), rt.resumed)
	}
}
