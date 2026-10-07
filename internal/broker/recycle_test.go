package broker

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/scion"
)

// recycleBroker builds a broker with one declared worker "worker" in phase
// (no record when phase is ""), the audit log captured, and the worker's
// workspace holding a work-product file. recyclable sets workers[].recyclable.
func recycleBroker(t *testing.T, phase string, recyclable bool) (*Broker, *fakeRuntime, *bytes.Buffer, WorkerSpec) {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "workers", "worker")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "deal.md"), []byte("work product"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", HostWorkspace: ws,
		TicketDir: "/run/user/501/lever/tickets/worker", Recyclable: recyclable}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}}
	if phase != "" {
		rt.agents[testInstanceProject] = []scion.Agent{{Slug: "worker", Phase: phase, ContainerStatus: "Exited (0) 1 minute ago"}}
	}
	var buf bytes.Buffer
	b := New(testConfig(t, withManager("test-manager", "appname"), withRuntime(rt, spec), withAudit(&buf)))
	return b, rt, &buf, spec
}

const recycleBody = `{"worker":"worker","task":"close deal 42"}`

func TestWorkerRecycle_suspendedDeletesRecordAndStartsFresh(t *testing.T) {
	for _, phase := range []string{scion.PhaseSuspended, scion.PhaseStopped, scion.PhaseError} {
		t.Run(phase, func(t *testing.T) {
			b, rt, audit, spec := recycleBroker(t, phase, true)
			rt.staged = map[string][][]byte{"worker": {[]byte("old ticket")}}
			b.reenrolTries["worker"] = 3

			rec := callWorker(t, b, "/worker/recycle", recycleBody, "test-manager")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
			}
			if len(rt.purged) != 1 || rt.purged[0] != "worker" {
				t.Fatalf("purged = %v, want [worker]", rt.purged)
			}
			if len(rt.started) != 1 || rt.started[0].Task != "close deal 42" || rt.started[0].Worker != "worker" {
				t.Fatalf("started = %+v, want one fresh start with the new task", rt.started)
			}
			if len(rt.resumed)+len(rt.resumeForced) != 0 {
				t.Fatal("a recycle must never resume the old record")
			}
			// The old ticket went with the purge; the start staged one fresh one.
			if n := len(rt.staged["worker"]); n != 1 {
				t.Fatalf("staged tickets = %d, want exactly the fresh one", n)
			}
			if bs := rt.lastStaged(t, "worker"); bs.AgentCN != "worker" || bs.Ticket == "" {
				t.Fatalf("fresh ticket = %+v", bs)
			}
			if got, err := os.ReadFile(filepath.Join(spec.HostWorkspace, "deal.md")); err != nil || string(got) != "work product" {
				t.Fatalf("workspace touched: %q, %v", got, err)
			}
			if b.reenrolTries["worker"] != 0 {
				t.Fatalf("healer attempts = %d, want reset to 0", b.reenrolTries["worker"])
			}
			log := audit.String()
			for _, want := range []string{
				"recycle worker: record deleted (phase " + phase + "), workspace kept",
				`task 13 bytes`, "decision=allow", "detail=\"start worker\"",
			} {
				if !strings.Contains(log, want) {
					t.Errorf("audit missing %q:\n%s", want, log)
				}
			}
		})
	}
}

func TestWorkerRecycle_auditBoundsTheTask(t *testing.T) {
	b, _, audit, _ := recycleBroker(t, scion.PhaseSuspended, true)
	task := "deal 7: " + strings.Repeat("SECRET-TAIL ", 40)
	rec := callWorker(t, b, "/worker/recycle", `{"worker":"worker","task":"`+task+`"}`, "test-manager")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	log := audit.String()
	if strings.Contains(log, task) || strings.Count(log, "SECRET-TAIL") > 5 {
		t.Fatalf("the audit log carries the whole task:\n%s", log)
	}
	if !strings.Contains(log, "deal 7: SECRET-TAIL") {
		t.Fatalf("the audit log lacks the task's start:\n%s", log)
	}
}

func TestWorkerRecycle_noRecordStartsFreshWithoutPurge(t *testing.T) {
	b, rt, audit, _ := recycleBroker(t, "", true)
	rec := callWorker(t, b, "/worker/recycle", recycleBody, "test-manager")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(rt.purged) != 0 || len(rt.started) != 1 {
		t.Fatalf("purged %v, started %d: want no purge and one start", rt.purged, len(rt.started))
	}
	if !strings.Contains(audit.String(), "recycle worker: no record, starting fresh") {
		t.Fatalf("audit:\n%s", audit.String())
	}
}

func TestWorkerRecycle_refusesNonRecyclableWorker(t *testing.T) {
	b, rt, audit, _ := recycleBroker(t, scion.PhaseSuspended, false)
	rec := callWorker(t, b, "/worker/recycle", recycleBody, "test-manager")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "recyclable: true") {
		t.Fatalf("body must name the key to set: %s", rec.Body.String())
	}
	assertNothingRecycled(t, rt)
	if !strings.Contains(audit.String(), "recycle worker: not recyclable") || !strings.Contains(audit.String(), "decision=deny") {
		t.Fatalf("audit:\n%s", audit.String())
	}
}

func TestWorkerRecycle_refusesTheManager(t *testing.T) {
	// By its CN and by its slug: neither is a declared worker.
	for _, name := range []string{"test-manager", "appname"} {
		b, rt, _, _ := recycleBroker(t, scion.PhaseSuspended, true)
		rec := callWorker(t, b, "/worker/recycle", `{"worker":"`+name+`","task":"x"}`, "test-manager")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403", name, rec.Code)
		}
		assertNothingRecycled(t, rt)
	}
	// A spec that carries the manager's slug (config validation refuses one;
	// the broker checks again).
	rt := &fakeRuntime{agents: map[string][]scion.Agent{testInstanceProject: {{Slug: "appname", Phase: "suspended"}}}}
	var buf bytes.Buffer
	b := New(testConfig(t, withManager("test-manager", "appname"),
		withRuntime(rt, WorkerSpec{Name: "appname", Recyclable: true}), withAudit(&buf)))
	rec := callWorker(t, b, "/worker/recycle", `{"worker":"appname","task":"x"}`, "test-manager")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("manager-slug spec: status = %d, want 403", rec.Code)
	}
	assertNothingRecycled(t, rt)
	if !strings.Contains(buf.String(), "the manager is never recycled") {
		t.Fatalf("audit:\n%s", buf.String())
	}
}

func TestWorkerRecycle_refusesAWorkerCaller(t *testing.T) {
	b, rt, _, _ := recycleBroker(t, scion.PhaseSuspended, true)
	rec := callWorker(t, b, "/worker/recycle", recycleBody, "worker")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	assertNothingRecycled(t, rt)
}

func TestWorkerRecycle_refusesRevokedManager(t *testing.T) {
	b, rt, audit, _ := recycleBroker(t, scion.PhaseSuspended, true)
	b.Revoke("test-manager")
	rec := callWorker(t, b, "/worker/recycle", recycleBody, "test-manager")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	assertNothingRecycled(t, rt)
	if !strings.Contains(audit.String(), "revoked") {
		t.Fatalf("audit:\n%s", audit.String())
	}
}

func TestWorkerRecycle_refusesALiveOrChangingRecord(t *testing.T) {
	for _, tc := range []struct{ phase, container string }{
		{"running", "Up 5 minutes"},
		{"running", "Exited (1) 1 minute ago"}, // the hub still calls it running
		{"starting", ""},
		{"resumed", "Up 1 second"},
		{"stopping", ""},
		{"made-up-by-the-agent", ""},
	} {
		t.Run(tc.phase+"/"+tc.container, func(t *testing.T) {
			b, rt, audit, _ := recycleBroker(t, "", true)
			rt.agents[testInstanceProject] = []scion.Agent{{Slug: "worker", Phase: tc.phase, ContainerStatus: tc.container}}
			rec := callWorker(t, b, "/worker/recycle", recycleBody, "test-manager")
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "agent stop worker") || !strings.Contains(rec.Body.String(), "Nothing was deleted") {
				t.Fatalf("body: %s", rec.Body.String())
			}
			assertNothingRecycled(t, rt)
			if strings.Contains(audit.String(), "made-up-by-the-agent") || strings.Contains(rec.Body.String(), "made-up-by-the-agent") {
				t.Fatal("an agent-set phase was echoed")
			}
		})
	}
}

func TestWorkerRecycle_refusesWhileAnotherLifecycleOpHoldsTheWorker(t *testing.T) {
	b, rt, audit, _ := recycleBroker(t, scion.PhaseSuspended, true)
	unlock, err := b.lockWorker(t.Context(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	rec := callWorker(t, b, "/worker/recycle", recycleBody, "test-manager")
	unlock()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	assertNothingRecycled(t, rt)
	if rt.listCalls != 0 {
		t.Fatalf("list calls = %d: a busy refusal must not read the phase", rt.listCalls)
	}
	if !strings.Contains(audit.String(), "recycle worker: busy") {
		t.Fatalf("audit:\n%s", audit.String())
	}
	// The lock was released: the next recycle goes ahead.
	if rec := callWorker(t, b, "/worker/recycle", recycleBody, "test-manager"); rec.Code != http.StatusOK {
		t.Fatalf("after unlock: status = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestWorkerRecycle_rateLimitedPerWorker(t *testing.T) {
	b, rt, audit, _ := recycleBroker(t, scion.PhaseSuspended, true)
	if rec := callWorker(t, b, "/worker/recycle", recycleBody, "test-manager"); rec.Code != http.StatusOK {
		t.Fatalf("first: status = %d (%s)", rec.Code, rec.Body.String())
	}
	// The manager stops the new worker and recycles again at once.
	rt.started = nil
	rt.agents[testInstanceProject] = []scion.Agent{{Slug: "worker", Phase: "stopped"}}
	rec := callWorker(t, b, "/worker/recycle", recycleBody, "test-manager")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second: status = %d, want 429 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	if len(rt.purged) != 1 || len(rt.started) != 0 {
		t.Fatalf("purged %v, started %d after the refusal: want the first purge only", rt.purged, len(rt.started))
	}
	if !strings.Contains(audit.String(), "recycle worker: rate limited") {
		t.Fatalf("audit:\n%s", audit.String())
	}
}

func TestWorkerRecycle_refusalsDoNotSpendTheRate(t *testing.T) {
	b, rt, _, _ := recycleBroker(t, "running", true)
	for range 3 {
		if rec := callWorker(t, b, "/worker/recycle", recycleBody, "test-manager"); rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
	}
	rt.agents[testInstanceProject] = []scion.Agent{{Slug: "worker", Phase: "stopped"}}
	if rec := callWorker(t, b, "/worker/recycle", recycleBody, "test-manager"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestWorkerRecycle_purgeFailureKeepsRecordAndStartsNothing(t *testing.T) {
	b, rt, audit, _ := recycleBroker(t, scion.PhaseSuspended, true)
	rt.purgeErr = errors.New("hub said no")
	rec := callWorker(t, b, "/worker/recycle", recycleBody, "test-manager")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if len(rt.started) != 0 || len(rt.staged["worker"]) != 0 {
		t.Fatal("nothing may start after a failed purge")
	}
	if !strings.Contains(audit.String(), "recycle worker: purge (phase suspended): hub said no") {
		t.Fatalf("audit:\n%s", audit.String())
	}
	// Nothing was deleted, so the retry is not rate limited.
	rt.purgeErr = nil
	if rec := callWorker(t, b, "/worker/recycle", recycleBody, "test-manager"); rec.Code != http.StatusOK {
		t.Fatalf("retry: status = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestWorkerRecycle_ticketRemovalFailureStillStarts(t *testing.T) {
	b, rt, audit, _ := recycleBroker(t, scion.PhaseSuspended, true)
	rt.ticketErr = errors.New("rm failed")
	rec := callWorker(t, b, "/worker/recycle", recycleBody, "test-manager")
	if rec.Code != http.StatusOK || len(rt.started) != 1 {
		t.Fatalf("status = %d, started %d", rec.Code, len(rt.started))
	}
	if !strings.Contains(audit.String(), "removing the staged ticket: rm failed") {
		t.Fatalf("audit:\n%s", audit.String())
	}
}

func TestWorkerRecycle_badTaskOrInstructionsDeleteNothing(t *testing.T) {
	b, rt, _, _ := recycleBroker(t, scion.PhaseSuspended, true)
	if rec := callWorker(t, b, "/worker/recycle", `{"worker":"worker","task":"--model=x"}`, "test-manager"); rec.Code != http.StatusBadRequest {
		t.Fatalf("flag-shaped task: status = %d, want 400", rec.Code)
	}
	big := strings.Repeat("a", 17*1024)
	if rec := callWorker(t, b, "/worker/recycle", `{"worker":"worker","task":"`+big+`"}`, "test-manager"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized task: status = %d, want 413", rec.Code)
	}
	assertNothingRecycled(t, rt)

	b2, rt2, _, spec := recycleBroker(t, scion.PhaseSuspended, true)
	spec.InstructionsPath = filepath.Join(t.TempDir(), "missing.md")
	b2.workers["worker"] = spec
	if rec := callWorker(t, b2, "/worker/recycle", recycleBody, "test-manager"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("missing instructions: status = %d, want 500", rec.Code)
	}
	assertNothingRecycled(t, rt2)
}

func TestWorkerRecycle_withoutPurgeChannelFailsClosed(t *testing.T) {
	_, rt, _, spec := recycleBroker(t, scion.PhaseSuspended, true)
	cfg := testConfig(t, withManager("test-manager", ""), withRuntime(rt, spec))
	cfg.Dispatch.Purge = nil
	b := New(cfg)
	if rec := callWorker(t, b, "/worker/recycle", recycleBody, "test-manager"); rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	assertNothingRecycled(t, rt)
}

func TestWorkerStartConflictPointsARecyclableWorkerAtRecycle(t *testing.T) {
	b, _, _, _ := recycleBroker(t, scion.PhaseSuspended, true)
	rec := callWorker(t, b, "/worker/start", recycleBody, "test-manager")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "lever-manager agent recycle worker") {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func assertNothingRecycled(t *testing.T, rt *fakeRuntime) {
	t.Helper()
	if len(rt.purged) != 0 || len(rt.started) != 0 || len(rt.resumed) != 0 || len(rt.resumeForced) != 0 {
		t.Fatalf("purged %v, started %d, resumed %v/%v: want no lifecycle call", rt.purged, len(rt.started), rt.resumed, rt.resumeForced)
	}
}
