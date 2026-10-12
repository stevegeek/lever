package broker

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/scion"
)

// capBroker declares workers a, b and c (a fresh start of each needs a host
// workspace) under manager slug "appname", with broker.max_running_workers
// set to max and the hub showing agents.
func capBroker(t *testing.T, max int, agents ...scion.Agent) (*Broker, *fakeRuntime, *bytes.Buffer) {
	t.Helper()
	var specs []WorkerSpec
	for _, n := range []string{"a", "b", "c"} {
		specs = append(specs, WorkerSpec{Name: n, WorkspaceSubdir: "workers/" + n,
			HostWorkspace: t.TempDir() + "/workers/" + n, TicketDir: "/run/user/501/lever/tickets/" + n, Recyclable: true})
	}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: agents}}
	var buf bytes.Buffer
	b := New(testConfig(t, withManager("test-manager", "appname"), withRuntime(rt, specs...), withAudit(&buf),
		func(c *Config) { c.Dispatch.MaxRunningWorkers = max }))
	return b, rt, &buf
}

func TestCountsAsRunning(t *testing.T) {
	for _, c := range []struct {
		phase, status string
		want          bool
	}{
		{"running", "Up 1 second", true},
		{"running", "", true},
		{"starting", "", true},
		{"provisioning", "", true},
		{"resumed", "", true},
		{"stopping", "", true},
		{"some new phase", "", true}, // unrecognised: counts (fail closed)
		{"suspended", "", false},
		{"stopped", "Exited (0) 1 minute ago", false},
		{"error", "Exited (1) 1 minute ago", false},
		{"", "", false},
		{"suspended", "Up 3 hours", true}, // a live container holds a slot whatever the phase says
	} {
		if got := countsAsRunning(c.phase, c.status); got != c.want {
			t.Errorf("countsAsRunning(%q, %q) = %t, want %t", c.phase, c.status, got, c.want)
		}
	}
}

// With the cap reached, a fresh start is refused by name before anything is
// staged or started; the message names the cap and the workers that hold it.
func TestWorkerCapRefusesFreshStart(t *testing.T) {
	b, rt, audit := capBroker(t, 1,
		scion.Agent{Slug: "appname", Phase: "running"}, // the manager never counts
		scion.Agent{Slug: "a", Phase: "running", ContainerStatus: "Up 1 minute"})
	rec := callWorker(t, b, "/worker/start", `{"worker":"b","task":"go"}`, "test-manager")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"worker b was not started", "broker.max_running_workers is 1", "1 worker(s) are running or starting (a)", "Nothing was changed"} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q lacks %q", body, want)
		}
	}
	if len(rt.started) != 0 || len(rt.staged) != 0 || len(rt.envSets) != 0 {
		t.Fatalf("a refused start changed something: started=%d staged=%d env=%d", len(rt.started), len(rt.staged), len(rt.envSets))
	}
	if !strings.Contains(audit.String(), "decision=deny") || !strings.Contains(audit.String(), "max_running_workers") {
		t.Fatalf("audit lacks the denial:\n%s", audit.String())
	}
}

// Below the cap the start goes ahead; suspended, stopped and error records
// and the manager do not count.
func TestWorkerCapAdmitsBelowCap(t *testing.T) {
	b, rt, _ := capBroker(t, 2,
		scion.Agent{Slug: "appname", Phase: "running", ContainerStatus: "Up 1 hour"},
		scion.Agent{Slug: "a", Phase: "running", ContainerStatus: "Up 1 minute"},
		scion.Agent{Slug: "c", Phase: "suspended"})
	rec := callWorker(t, b, "/worker/start", `{"worker":"b","task":"go"}`, "test-manager")
	if rec.Code != http.StatusOK || len(rt.started) != 1 {
		t.Fatalf("status = %d started=%d (%s)", rec.Code, len(rt.started), rec.Body.String())
	}
	if len(b.admitting) != 0 {
		t.Fatalf("the admission was not released: %v", b.admitting)
	}
}

// A worker never counts against itself: its own record (here one the hub
// still calls running, about to be resumed --force) is left out of the count.
func TestWorkerCapDoesNotCountTheWorkerItself(t *testing.T) {
	b, _, _ := capBroker(t, 1, scion.Agent{Slug: "a", Phase: "running", ContainerStatus: "Up 1 minute"})
	release, err := b.admitWorker(context.Background(), "a")
	if err != nil {
		t.Fatalf("admission of the worker holding the only slot: %v", err)
	}
	release()
}

// The resume verb, the remote wake and the start route's resume arm all take
// a slot.
func TestWorkerCapRefusesResumeAndWake(t *testing.T) {
	agents := []scion.Agent{
		{Slug: "a", Phase: "running", ContainerStatus: "Up 1 minute"},
		{Slug: "b", Phase: "suspended"},
	}
	t.Run("resume verb", func(t *testing.T) {
		b, rt, _ := capBroker(t, 1, agents...)
		rec := callWorker(t, b, "/worker/resume", `{"worker":"b"}`, "test-manager")
		if rec.Code != http.StatusConflict || len(rt.resumed) != 0 || len(rt.staged) != 0 {
			t.Fatalf("status = %d resumed=%v staged=%d (%s)", rec.Code, rt.resumed, len(rt.staged), rec.Body.String())
		}
	})
	t.Run("start of a suspended record", func(t *testing.T) {
		b, rt, _ := capBroker(t, 1, agents...)
		rec := callWorker(t, b, "/worker/start", `{"worker":"b","task":""}`, "test-manager")
		if rec.Code != http.StatusConflict || len(rt.resumed) != 0 {
			t.Fatalf("status = %d resumed=%v (%s)", rec.Code, rt.resumed, rec.Body.String())
		}
	})
	t.Run("wake", func(t *testing.T) {
		b, rt, _ := capBroker(t, 1, agents...)
		rec := postWake(t, b, `{"worker":"b","login":"c@x","tier":"operator"}`)
		if rec.Code != http.StatusConflict || len(rt.resumed) != 0 {
			t.Fatalf("status = %d resumed=%v (%s)", rec.Code, rt.resumed, rec.Body.String())
		}
		// A chat-page login is not told which workers run or what the cap is.
		if body := rec.Body.String(); strings.Contains(body, "max_running_workers") || strings.Contains(body, "(a)") {
			t.Fatalf("wake refusal leaks the cap detail: %q", body)
		}
	})
	t.Run("resume below the cap", func(t *testing.T) {
		b, rt, _ := capBroker(t, 2, agents...)
		rec := callWorker(t, b, "/worker/resume", `{"worker":"b"}`, "test-manager")
		if rec.Code != http.StatusOK || len(rt.resumed) != 1 {
			t.Fatalf("status = %d resumed=%v (%s)", rec.Code, rt.resumed, rec.Body.String())
		}
	})
}

// A recycle at the cap is refused before the purge: nothing is deleted.
func TestWorkerCapRefusesRecycleBeforePurge(t *testing.T) {
	b, rt, _ := capBroker(t, 1,
		scion.Agent{Slug: "a", Phase: "running", ContainerStatus: "Up 1 minute"},
		scion.Agent{Slug: "b", Phase: "stopped", ContainerStatus: "Exited (0) 1 minute ago"})
	rec := callWorker(t, b, "/worker/recycle", `{"worker":"b","task":"new work"}`, "test-manager")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(rt.purged) != 0 || len(rt.started) != 0 {
		t.Fatalf("a refused recycle deleted or started: purged=%v started=%d", rt.purged, len(rt.started))
	}
}

// Starts admitted but not yet in the records count: two at once cannot both
// take the last slot.
func TestWorkerCapCountsAdmittedStarts(t *testing.T) {
	b, _, _ := capBroker(t, 1)
	release, err := b.admitWorker(context.Background(), "a")
	if err != nil {
		t.Fatalf("first admission: %v", err)
	}
	if _, err := b.admitWorker(context.Background(), "b"); !errors.Is(err, ErrWorkerCap) {
		t.Fatalf("second admission while the first is in flight: err = %v, want ErrWorkerCap", err)
	}
	// The same worker admitted twice (recycle, then its fresh start) is one slot.
	again, err := b.admitWorker(context.Background(), "a")
	if err != nil {
		t.Fatalf("re-admission of the same worker: %v", err)
	}
	again()
	if _, err := b.admitWorker(context.Background(), "b"); !errors.Is(err, ErrWorkerCap) {
		t.Fatal("the inner release freed the outer admission's slot")
	}
	release()
	if r, err := b.admitWorker(context.Background(), "b"); err != nil {
		t.Fatalf("after release: %v", err)
	} else {
		r()
	}
}

// Undeclared agents in the project count too, and are named by number.
func TestWorkerCapCountsUndeclaredAgents(t *testing.T) {
	b, _, _ := capBroker(t, 1, scion.Agent{Slug: "leftover", Phase: "running"})
	_, err := b.admitWorker(context.Background(), "a")
	if !errors.Is(err, ErrWorkerCap) || !strings.Contains(err.Error(), "1 other agent(s)") {
		t.Fatalf("err = %v", err)
	}
}

// No cap: nothing is read. A failed read under a cap refuses (fail closed).
func TestWorkerCapOffAndListFailure(t *testing.T) {
	b, rt, _ := capBroker(t, 0, scion.Agent{Slug: "a", Phase: "running"}, scion.Agent{Slug: "b", Phase: "running"})
	release, err := b.admitWorker(context.Background(), "c")
	if err != nil || rt.listCalls != 0 {
		t.Fatalf("no cap: err=%v listCalls=%d", err, rt.listCalls)
	}
	release()

	b, rt, _ = capBroker(t, 3)
	rt.listErr = errors.New("hub down")
	if _, err := b.admitWorker(context.Background(), "a"); err == nil || errors.Is(err, ErrWorkerCap) {
		t.Fatalf("list failure: err = %v, want a non-cap error", err)
	}
}

// The re-enrol healer's resume of a worker that is not running takes a slot;
// its bounce of a running one does not ask.
func TestWorkerCapHealerResume(t *testing.T) {
	b, rt, audit := capBroker(t, 1,
		scion.Agent{Slug: "a", Phase: "running", ContainerStatus: "Up 1 minute"},
		scion.Agent{Slug: "b", Phase: "suspended"})
	rt.staticPhases = true
	if _, ok := b.bounceForReenrol(context.Background(), "b", "b"); ok {
		t.Fatal("the healer resumed a worker past the cap")
	}
	if len(rt.resumed) != 0 || !strings.Contains(audit.String(), "max_running_workers") {
		t.Fatalf("resumed=%v audit:\n%s", rt.resumed, audit.String())
	}
	if verb, ok := b.bounceForReenrol(context.Background(), "a", "a"); !ok || verb != "suspend+resume" {
		t.Fatalf("bounce of a running worker: verb=%q ok=%t", verb, ok)
	}
}

// workers[].resources reaches the fresh start.
func TestWorkerStartCarriesResources(t *testing.T) {
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", HostWorkspace: t.TempDir() + "/w",
		TicketDir: "/run/user/501/lever/tickets/worker", Resources: scion.Resources{Memory: "1536Mi", CPU: "1"}}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}}
	b := newTestBroker(t, rt, spec)
	rec := callWorker(t, b, "/worker/start", `{"worker":"worker","task":"go"}`, "test-manager")
	if rec.Code != http.StatusOK || len(rt.started) != 1 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := rt.started[0].Resources; got != spec.Resources {
		t.Fatalf("Start resources = %+v, want %+v", got, spec.Resources)
	}
}
