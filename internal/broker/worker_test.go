package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/scion"
)

// fakeRuntime records calls and returns scripted results; satisfies WorkerRuntime.
type fakeRuntime struct {
	agents         map[string][]scion.Agent // project -> agents (for List)
	started        []scion.StartOpts
	resumed        []string
	resumeProj     []string
	resumeForced   []string
	resumeForceErr error
	stopped        []string
	stopProj       []string
	suspend        []string
	suspendProj    []string
	envSets        []string
	envSetProj     []string
	startErr       error
	listCalls      int      // total List invocations, to assert the fan-out is collapsed
	listProjects   []string // project arg of every List call
	// staticPhases disables the acted->running modelling below: List always
	// returns the seeded agents. Healer tests need a phase that persists
	// across repeated heal attempts.
	staticPhases bool
	// exitedAfterStart, when set, makes List report a present-but-DEAD worker
	// ("scratch") once Start has run — so the post-start liveness poll observes a
	// crash-looped container (Phase "running", ContainerStatus "Exited (1)").
	// Before Start it stays absent, so handleWorkerStart still takes the Start path.
	exitedAfterStart bool
	// dieAfterLists, when > 0, models lever#31: after a Start/Resume the record
	// reads running/Up for this many List calls, then flips to error/Exited —
	// the harness died after scion reported success.
	dieAfterLists int
	actedLists    int
}

func (f *fakeRuntime) List(_ context.Context, project string) ([]scion.Agent, error) {
	f.listCalls++
	f.listProjects = append(f.listProjects, project)
	// After a Start/Resume, model scion bringing the worker up: the record shows
	// running + a live container, so the post-start liveness poll succeeds. This
	// mirrors the real runtime — the pre-action `agents` map is the observe-first
	// state; the poll sees the result of the action. exitedAfterStart flips it to
	// a crash-loop (present but dead) to exercise the liveness-timeout path.
	if name, acted := f.lastActed(); acted && !f.staticPhases {
		if f.exitedAfterStart {
			return []scion.Agent{{Slug: name, Phase: "running", ContainerStatus: "Exited (1) 2 seconds ago"}}, nil
		}
		if f.dieAfterLists > 0 {
			f.actedLists++
			if f.actedLists > f.dieAfterLists {
				return []scion.Agent{{Slug: name, Phase: "error", ContainerStatus: "Exited (1) 2 seconds ago"}}, nil
			}
		}
		return []scion.Agent{{Slug: name, Phase: "running", ContainerStatus: "Up 1 second"}}, nil
	}
	return f.agents[project], nil
}

// lastActed reports the worker name a Start/Resume acted on, and whether any
// such action has happened yet (so pre-action List calls return the seed state).
func (f *fakeRuntime) lastActed() (string, bool) {
	if len(f.started) > 0 {
		return f.started[len(f.started)-1].Worker, true
	}
	if len(f.resumed) > 0 {
		return f.resumed[len(f.resumed)-1], true
	}
	if len(f.resumeForced) > 0 {
		return f.resumeForced[len(f.resumeForced)-1], true
	}
	return "", false
}
func (f *fakeRuntime) Start(_ context.Context, o scion.StartOpts) error {
	f.started = append(f.started, o)
	return f.startErr
}
func (f *fakeRuntime) Resume(_ context.Context, worker, project string) error {
	f.resumed = append(f.resumed, worker)
	f.resumeProj = append(f.resumeProj, project)
	return nil
}
func (f *fakeRuntime) ResumeForce(_ context.Context, worker, project string) error {
	f.resumeForced = append(f.resumeForced, worker)
	return f.resumeForceErr
}

func (f *fakeRuntime) Stop(_ context.Context, worker, project string) error {
	f.stopped = append(f.stopped, worker)
	f.stopProj = append(f.stopProj, project)
	return nil
}
func (f *fakeRuntime) Suspend(_ context.Context, worker, project string) error {
	f.suspend = append(f.suspend, worker)
	f.suspendProj = append(f.suspendProj, project)
	return nil
}
func (f *fakeRuntime) EnvSet(_ context.Context, project, _, _ string) error {
	f.envSets = append(f.envSets, "set")
	f.envSetProj = append(f.envSetProj, project)
	return nil
}
func (f *fakeRuntime) Message(_ context.Context, _ scion.MsgOpts) error { return nil }
func (f *fakeRuntime) Inbox(_ context.Context, _ bool, _ string) ([]scion.Event, error) {
	return nil, nil
}

// testInstanceProject is the constant instance project (-g) used across
// worker/msg tests, matching the single-project model: every worker is an
// agent within this one project, distinguished by --workspace subdir.
const testInstanceProject = "/lever"

func TestWorkerSpecLookup(t *testing.T) {
	b := New(Config{Dispatch: DispatchConfig{
		Workers: []WorkerSpec{{Name: "worker", WorkspaceSubdir: "workers/worker"}},
	}})
	if _, ok := b.workerSpec("worker"); !ok {
		t.Fatal("expected worker spec present")
	}
	if _, ok := b.workerSpec("nope"); ok {
		t.Fatal("expected absent spec to be missing")
	}
}

// callWorker drives a handler with a synthetic verified client cert CN.
func callWorker(t *testing.T, b *Broker, path, body, cn string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.TLS = fakeTLSWithCN(cn)
	rec := httptest.NewRecorder()
	b.JailHandler().ServeHTTP(rec, req)
	return rec
}

func TestWorkerStart_absent_provisionsStagesStarts(t *testing.T) {
	dir := t.TempDir()
	hostWorkspace := filepath.Join(t.TempDir(), "workers", "worker") // does NOT exist yet
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", HostWorkspace: hostWorkspace,
		BootstrapDir: filepath.Join(dir, ".lever"), Image: "img:1", Model: "claude-opus-5", APIKey: true}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}} // absent
	b := newTestBroker(t, rt, spec)

	rec := callWorker(t, b, "/worker/start", `{"worker":"worker","task":"do it"}`, "test-manager")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	// the host workspace subdir is created before dispatch
	if fi, err := os.Stat(hostWorkspace); err != nil || !fi.IsDir() {
		t.Fatalf("host workspace not created: %v", err)
	}
	// bootstrap staged 0600 with the broker CA/URL and the worker CN
	raw, err := os.ReadFile(filepath.Join(spec.BootstrapDir, "bootstrap.json"))
	if err != nil {
		t.Fatalf("bootstrap not staged: %v", err)
	}
	var bs struct {
		Ticket    string `json:"ticket"`
		BrokerCA  string `json:"broker_ca"`
		BrokerURL string `json:"broker_url"`
		AgentCN   string `json:"agent_cn"`
	}
	_ = json.Unmarshal(raw, &bs)
	if bs.Ticket == "" || bs.BrokerCA != "CA-PEM" || bs.BrokerURL != "https://10.0.0.2:8080" || bs.AgentCN != "worker" {
		t.Fatalf("bad bootstrap: %+v", bs)
	}
	fi, _ := os.Stat(filepath.Join(spec.BootstrapDir, "bootstrap.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("bootstrap perms = %v, want 0600", fi.Mode().Perm())
	}
	// scion start called with the constant instance project (-g) + per-worker
	// --workspace subdir (no longer equal to each other) + api-key + image +
	// model. Image and Model are config-resolved host-side: the manager names a
	// worker and never supplies either, so the spec's values must be what
	// reaches scion.
	if len(rt.started) != 1 {
		t.Fatalf("start calls = %d, want 1", len(rt.started))
	}
	got := rt.started[0]
	if got.Project != testInstanceProject || got.WorkspaceSubdir != "workers/worker" ||
		got.Worker != "worker" || got.Image != "img:1" || got.Model != "claude-opus-5" || !got.APIKey {
		t.Fatalf("bad StartOpts: %+v", got)
	}
	if got.Task != "do it" {
		t.Fatalf("StartOpts.Task = %q, want \"do it\"", got.Task)
	}
	if len(rt.envSets) != 1 || rt.envSetProj[0] != testInstanceProject {
		t.Fatalf("EnvSet calls = %d (proj %v), want 1 at the instance project", len(rt.envSets), rt.envSetProj)
	}
}

func TestWorkerStart_running_isNoop(t *testing.T) {
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", BootstrapDir: t.TempDir()}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "worker", Phase: "running"}},
	}}
	b := newTestBroker(t, rt, spec)
	rec := callWorker(t, b, "/worker/start", `{"worker":"worker"}`, "test-manager")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(rt.started) != 0 || len(rt.resumed) != 0 {
		t.Fatal("running worker must be a no-op")
	}
}

func TestWorkerStart_suspended_resumesNoReprovision(t *testing.T) {
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker",
		BootstrapDir: filepath.Join(t.TempDir(), ".lever")}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "worker", Phase: "suspended"}},
	}}
	b := newTestBroker(t, rt, spec)
	rec := callWorker(t, b, "/worker/start", `{"worker":"worker"}`, "test-manager")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(rt.resumed) != 1 || len(rt.started) != 0 {
		t.Fatalf("expected resume-only; resumed=%d started=%d", len(rt.resumed), len(rt.started))
	}
	if rt.resumeProj[0] != testInstanceProject {
		t.Fatalf("resume project = %q, want the instance project %q", rt.resumeProj[0], testInstanceProject)
	}
	// Resume DOES stage a fresh one-use ticket (the 0.10-era manager fix,
	// mirrored): a worker resumed after its leaf/ticket lifetime would
	// otherwise re-enrol with a spent ticket and wedge into phase=error
	// (live-hit 2026-07-31). Harmless when the leaf is still valid — boot
	// skips enrol and the ticket ages out unspent.
	if _, err := os.Stat(filepath.Join(spec.BootstrapDir, "bootstrap.json")); err != nil {
		t.Fatal("resume must stage a fresh bootstrap ticket")
	}
}

// TestWorkerStart_terminalPhase_resumesNoReprovision verifies that any non-empty,
// non-running phase (e.g. "exited") takes the resume path — a fresh ticket is
// staged (see the suspended-phase test) but scion start is never re-run.
func TestWorkerStart_terminalPhase_resumesNoReprovision(t *testing.T) {
	bootstrapDir := filepath.Join(t.TempDir(), ".lever")
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker",
		BootstrapDir: bootstrapDir}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "worker", Phase: "exited"}},
	}}
	b := newTestBroker(t, rt, spec)
	rec := callWorker(t, b, "/worker/start", `{"worker":"worker"}`, "test-manager")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	// must resume, not start
	if len(rt.resumed) != 1 {
		t.Fatalf("resumed = %d, want 1", len(rt.resumed))
	}
	if len(rt.started) != 0 {
		t.Fatalf("started = %d, want 0 (must not re-provision)", len(rt.started))
	}
	// fresh ticket staged for the re-enrol-on-boot path
	if _, err := os.Stat(filepath.Join(bootstrapDir, "bootstrap.json")); err != nil {
		t.Fatal("terminal-phase resume must stage a fresh bootstrap ticket")
	}
}

// An ERROR-phase worker record is recovered via resume --force (scion#895)
// with a fresh ticket staged — the 2026-07-31 wedge (spent-ticket enrol deny
// -> error phase -> 409 on every message) heals without a purge.
func TestWorkerStart_errorPhase_resumeForce(t *testing.T) {
	bootstrapDir := filepath.Join(t.TempDir(), ".lever")
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker",
		BootstrapDir: bootstrapDir}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "worker", Phase: "error"}},
	}}
	b := newTestBroker(t, rt, spec)
	rec := callWorker(t, b, "/worker/start", `{"worker":"worker"}`, "test-manager")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if len(rt.resumeForced) != 1 || len(rt.resumed) != 0 || len(rt.started) != 0 {
		t.Fatalf("verbs: forced=%d resumed=%d started=%d, want force-only",
			len(rt.resumeForced), len(rt.resumed), len(rt.started))
	}
	if _, err := os.Stat(filepath.Join(bootstrapDir, "bootstrap.json")); err != nil {
		t.Fatal("error-phase resume must stage a fresh bootstrap ticket")
	}
}

// A re-dispatch carrying a NEW task against a non-running record must REFUSE
// loudly (409, body points at purge) instead of silently resuming — because
// scion pins the task at creation, so a resume would replay the ORIGINAL task,
// not the new one.
func TestWorkerStartRefusesTaskMismatch(t *testing.T) {
	spec := WorkerSpec{Name: "scratch", WorkspaceSubdir: "workers/scratch",
		BootstrapDir: filepath.Join(t.TempDir(), ".lever")}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: "suspended"}},
	}}
	b := newTestBroker(t, rt, spec)

	rec := callWorker(t, b, "/worker/start", `{"worker":"scratch","task":"NEW TASK"}`, "test-manager")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "purge") {
		t.Fatalf("body must point at `lever worker purge`, got: %s", rec.Body.String())
	}
	if len(rt.resumed) != 0 {
		t.Fatalf("task-mismatch must NOT resume, got %d resume calls", len(rt.resumed))
	}
	// A resume with NO task is still allowed (idempotent bring-up).
	rec2 := callWorker(t, b, "/worker/start", `{"worker":"scratch"}`, "test-manager")
	if rec2.Code != http.StatusOK {
		t.Fatalf("taskless resume status = %d, want 200 (%s)", rec2.Code, rec2.Body.String())
	}
	if len(rt.resumed) != 1 {
		t.Fatalf("taskless resume must resume once, got %d", len(rt.resumed))
	}
}

// After a successful Start, an un-live container (crash-loop) must surface as a
// loud error, NOT a false {Phase:"running"}.
func TestWorkerStartLivenessTimeout(t *testing.T) {
	dir := t.TempDir()
	spec := WorkerSpec{Name: "scratch", WorkspaceSubdir: "workers/scratch", HostWorkspace: t.TempDir(),
		BootstrapDir: filepath.Join(dir, ".lever")}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}, exitedAfterStart: true} // absent, then Exited after Start
	b := newTestBroker(t, rt, spec)
	b.liveAttempts, b.liveInterval = 3, time.Millisecond

	rec := callWorker(t, b, "/worker/start", `{"worker":"scratch","task":"go"}`, "test-manager")

	if rec.Code == http.StatusOK {
		t.Fatalf("un-live container must NOT return 200; body: %s", rec.Body.String())
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"phase":"running"`) {
		t.Fatalf("must not report running for a dead container: %s", rec.Body.String())
	}
	if len(rt.started) != 1 {
		t.Fatalf("Start should have been attempted once, got %d", len(rt.started))
	}
}

// midPollListFailRuntime wraps fakeRuntime to fail the first failAfterAct List
// calls that occur AFTER a Start/Resume — modelling transient hub blips DURING
// the post-start liveness poll. The observe-first List (pre-action) still
// succeeds, matching the real sequence: create/resume already succeeded, so the
// hub is demonstrably up and a mid-poll List error is a hiccup, not a failure.
type midPollListFailRuntime struct {
	*fakeRuntime
	failAfterAct int
	postActLists int
}

func (r *midPollListFailRuntime) List(ctx context.Context, project string) ([]scion.Agent, error) {
	if _, acted := r.lastActed(); acted {
		r.postActLists++
		if r.postActLists <= r.failAfterAct {
			return nil, fmt.Errorf("hub: transient blip")
		}
	}
	return r.fakeRuntime.List(ctx, project)
}

// TestWorkerStartLivenessToleratesMidPollListErrors pins waitWorkerLive's
// mid-poll List tolerance: a transient List error during the post-start
// liveness poll must NOT abort the start — the failed attempt is consumed
// within the existing budget and the poll succeeds as soon as a List call
// reports the worker running/live. This behavior must survive the WaitAgentLive
// extraction (plan B3).
func TestWorkerStartLivenessToleratesMidPollListErrors(t *testing.T) {
	dir := t.TempDir()
	spec := WorkerSpec{Name: "scratch", WorkspaceSubdir: "workers/scratch", HostWorkspace: t.TempDir(),
		BootstrapDir: filepath.Join(dir, ".lever")}
	rt := &midPollListFailRuntime{
		fakeRuntime:  &fakeRuntime{agents: map[string][]scion.Agent{}}, // absent -> Start path
		failAfterAct: 2,                                                // two blips inside the liveness poll before a live record
	}
	b := newTestBroker(t, rt, spec)
	b.liveAttempts, b.liveInterval = 5, time.Millisecond

	rec := callWorker(t, b, "/worker/start", `{"worker":"scratch","task":"go"}`, "test-manager")

	if rec.Code != http.StatusOK {
		t.Fatalf("two transient mid-poll List blips must not fail worker start; status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"phase":"running"`) {
		t.Fatalf("worker should report running once its record goes live; body=%s", rec.Body.String())
	}
	if len(rt.started) != 1 {
		t.Fatalf("Start should have been attempted once, got %d", len(rt.started))
	}
}

// TestWorkerStartStageFailure forces stageBootstrap to fail — BootstrapDir sits
// directly under a read-only parent so its MkdirAll is denied — on BOTH the
// fresh-start (absent) and resume (existing non-running) paths, and asserts each
// returns 500 with body "stage error" and dispatches no scion verb. These
// stage-failure branches otherwise have zero coverage.
func TestWorkerStartStageFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		agents map[string][]scion.Agent
	}{
		{"fresh_start", map[string][]scion.Agent{}},
		{"resume", map[string][]scion.Agent{testInstanceProject: {{Slug: "worker", Phase: "suspended"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			if err := os.Chmod(parent, 0o500); err != nil {
				t.Fatal(err)
			}
			// Restore write before TempDir's own cleanup removes the tree (LIFO:
			// this runs before the removal registered at t.TempDir() time).
			t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
			spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker",
				HostWorkspace: t.TempDir(),
				BootstrapDir:  filepath.Join(parent, ".lever")} // MkdirAll denied under 0500 parent
			rt := &fakeRuntime{agents: tc.agents}
			b := newTestBroker(t, rt, spec)

			rec := callWorker(t, b, "/worker/start", `{"worker":"worker"}`, "test-manager")

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (%s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "stage error") {
				t.Fatalf("body = %q, want to contain \"stage error\"", rec.Body.String())
			}
			// A stage failure must abort before any scion start/resume runs.
			if len(rt.started) != 0 || len(rt.resumed) != 0 || len(rt.resumeForced) != 0 {
				t.Fatalf("stage-failure must not dispatch: started=%d resumed=%d forced=%d",
					len(rt.started), len(rt.resumed), len(rt.resumeForced))
			}
		})
	}
}

func TestWorkerStart_authz(t *testing.T) {
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", BootstrapDir: t.TempDir()}
	b := newTestBroker(t, &fakeRuntime{agents: map[string][]scion.Agent{}}, spec)
	// wrong CN
	if rec := callWorker(t, b, "/worker/start", `{"worker":"worker"}`, "intruder"); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong-CN status = %d, want 403", rec.Code)
	}
	// undeclared worker
	if rec := callWorker(t, b, "/worker/start", `{"worker":"ghost"}`, "test-manager"); rec.Code != http.StatusForbidden {
		t.Fatalf("undeclared worker status = %d, want 403", rec.Code)
	}
}

// Authentication runs BEFORE the body is decoded on every worker route: an
// intruder posting garbage gets 403, not 400 (no body-shape oracle), and the
// manager posting garbage gets 400 with a "bad body" audit line.
func TestWorkerRoutes_authBeforeDecode(t *testing.T) {
	for _, path := range []string{"/worker/start", "/worker/stop", "/worker/suspend", "/worker/resume"} {
		t.Run(path, func(t *testing.T) {
			b, _, audit := newMsgTestBroker(t, true)
			if rec := callWorker(t, b, path, `{`, "intruder"); rec.Code != http.StatusForbidden {
				t.Fatalf("intruder bad body: status = %d, want 403", rec.Code)
			}
			if rec := callWorker(t, b, path, `{`, "manager"); rec.Code != http.StatusBadRequest {
				t.Fatalf("manager bad body: status = %d, want 400", rec.Code)
			}
			if !strings.Contains(audit.String(), `detail="bad body"`) {
				t.Fatalf("audit missing bad body line: %s", audit.String())
			}
		})
	}
}

// TestWorkerList proves the list fan-out is collapsed to a SINGLE
// List(instanceProject) call that returns the whole fleet (multiple workers),
// not one call per declared worker.
func TestWorkerList(t *testing.T) {
	rt := &fakeRuntime{agents: map[string][]scion.Agent{
		testInstanceProject: {
			{Slug: "worker", Phase: "running", Activity: "building"},
			{Slug: "helper", Phase: "suspended"},
		},
	}}
	b := New(testConfig(t, withManager("test-manager", ""), withRuntime(rt,
		WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", BootstrapDir: t.TempDir()},
		WorkerSpec{Name: "helper", WorkspaceSubdir: "workers/helper", BootstrapDir: t.TempDir()},
	)))
	req := httptest.NewRequest("POST", "/worker/list", nil)
	req.TLS = fakeTLSWithCN("test-manager")
	rec := httptest.NewRecorder()
	b.JailHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rt.listCalls != 1 {
		t.Fatalf("List calls = %d, want exactly 1 (collapsed fan-out)", rt.listCalls)
	}
	if rt.listProjects[0] != testInstanceProject {
		t.Fatalf("List project = %q, want the instance project %q", rt.listProjects[0], testInstanceProject)
	}
	var out struct {
		Agents []scion.Agent `json:"agents"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Agents) != 2 {
		t.Fatalf("bad list: %+v", out.Agents)
	}
	// non-manager rejected
	req2 := httptest.NewRequest("POST", "/worker/list", nil)
	req2.TLS = fakeTLSWithCN("intruder")
	rec2 := httptest.NewRecorder()
	b.JailHandler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("intruder list = %d, want 403", rec2.Code)
	}
}

// TestWorkerNilRuntime_returns502 proves that when the scion runtime is unwired
// (nil) the worker handlers return 502, not a panic from a nil-interface call.
func TestWorkerNilRuntime_returns502(t *testing.T) {
	// Build a broker with an explicit nil runtime (no LEVER_JAIL_USER/UID env).
	// Runtime nil: unwired, simulates a manual `lever broker serve`.
	// Both verbs with the manager CN must return 502, not panic.
	assertNilRuntimeVerbs(t, "test-manager", http.StatusBadGateway)
}

// TestWorkerNilRuntime_authzPrecedence proves that even with nil runtime, an
// unauthenticated or non-manager caller gets 403 (authz runs before the nil check).
func TestWorkerNilRuntime_authzPrecedence(t *testing.T) {
	assertNilRuntimeVerbs(t, "intruder", http.StatusForbidden)
}

// assertNilRuntimeVerbs calls /worker/start and /worker/list as cn on a broker
// whose scion runtime is nil and pins the status both must answer.
func assertNilRuntimeVerbs(t *testing.T, cn string, want int) {
	t.Helper()
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", BootstrapDir: t.TempDir()}
	b := newTestBroker(t, nil, spec)
	for _, path := range []string{"/worker/start", "/worker/list"} {
		rec := callWorker(t, b, path, `{"worker":"worker","task":"go"}`, cn)
		if rec.Code != want {
			t.Fatalf("%s %s nil-runtime: status = %d, want %d", path, cn, rec.Code, want)
		}
	}
}

func TestWorkerLifecycleVerbs(t *testing.T) {
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", BootstrapDir: t.TempDir()}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}}
	b := newTestBroker(t, rt, spec)

	for _, tc := range []struct {
		path  string
		check func() bool
	}{
		{"/worker/stop", func() bool { return len(rt.stopped) == 1 && rt.stopProj[0] == testInstanceProject }},
		{"/worker/suspend", func() bool { return len(rt.suspend) == 1 && rt.suspendProj[0] == testInstanceProject }},
		{"/worker/resume", func() bool { return len(rt.resumed) == 1 && rt.resumeProj[0] == testInstanceProject }},
	} {
		rec := callWorker(t, b, tc.path, `{"worker":"worker"}`, "test-manager")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", tc.path, rec.Code)
		}
		if !tc.check() {
			t.Fatalf("%s did not dispatch the scion call", tc.path)
		}
	}
	// authz still enforced
	if rec := callWorker(t, b, "/worker/stop", `{"worker":"ghost"}`, "test-manager"); rec.Code != http.StatusForbidden {
		t.Fatalf("undeclared stop = %d, want 403", rec.Code)
	}
}

func TestWorkerStart_deniesRevokedManager(t *testing.T) {
	dir := t.TempDir()
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", HostWorkspace: t.TempDir(),
		BootstrapDir: filepath.Join(dir, ".lever"), Image: "img:1", APIKey: true}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}}
	b := newTestBroker(t, rt, spec)
	b.Revoke("test-manager")

	rec := callWorker(t, b, "/worker/start", `{"worker":"worker","task":"do it"}`, "test-manager")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoked manager dispatch: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	// No bootstrap staged, no start attempted.
	if _, err := os.Stat(filepath.Join(spec.BootstrapDir, "bootstrap.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("revoked dispatch must not stage bootstrap")
	}
}

// The worker's standing instructions come from the HOST file config resolved
// for it, read at dispatch, and travel to scion in StartOpts.Instructions —
// the manager names the worker and supplies the task, never the manual.
func TestWorkerStart_absent_passesInstructionsFromHostFile(t *testing.T) {
	dir := t.TempDir()
	manual := filepath.Join(dir, "worker-manual.md")
	if err := os.WriteFile(manual, []byte("# Worker manual\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", HostWorkspace: filepath.Join(t.TempDir(), "workers", "worker"),
		BootstrapDir: filepath.Join(dir, ".lever"), Image: "img:1", InstructionsPath: manual}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}} // absent
	b := newTestBroker(t, rt, spec)

	rec := callWorker(t, b, "/worker/start", `{"worker":"worker","task":"do it"}`, "test-manager")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if len(rt.started) != 1 {
		t.Fatalf("start calls = %d, want 1", len(rt.started))
	}
	if got := rt.started[0]; got.Instructions != "# Worker manual\n" || got.Task != "do it" {
		t.Fatalf("StartOpts must carry the manual as Instructions and the task unchanged: %+v", got)
	}
}

// A config that names a file the host cannot read is an operator error: fail
// before ANY side effect — no ticket staged, no scion start.
func TestWorkerStart_absent_missingInstructionsFileFailsBeforeStaging(t *testing.T) {
	dir := t.TempDir()
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", HostWorkspace: filepath.Join(t.TempDir(), "workers", "worker"),
		BootstrapDir: filepath.Join(dir, ".lever"), Image: "img:1", InstructionsPath: filepath.Join(dir, "absent.md")}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}} // absent
	b := newTestBroker(t, rt, spec)

	rec := callWorker(t, b, "/worker/start", `{"worker":"worker","task":"do it"}`, "test-manager")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", rec.Code, rec.Body.String())
	}
	if len(rt.started) != 0 {
		t.Fatalf("no start may happen without the instructions; got %d", len(rt.started))
	}
	if _, err := os.Stat(filepath.Join(spec.BootstrapDir, "bootstrap.json")); !os.IsNotExist(err) {
		t.Fatalf("no ticket may be staged when the instructions file is unreadable (stat err %v)", err)
	}
}

// A task past scion's argv cap is refused by name with 413 before the phase
// lookup, so the manager learns WHY instead of getting a generic runtime
// error for a container that exited "command too long" (lever#30).
func TestWorkerStart_oversizedTaskIs413WithoutStart(t *testing.T) {
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", HostWorkspace: filepath.Join(t.TempDir(), "workers", "worker"),
		BootstrapDir: filepath.Join(t.TempDir(), ".lever"), Image: "img:1"}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}} // absent
	b := newTestBroker(t, rt, spec)
	body, _ := json.Marshal(map[string]string{"worker": "worker", "task": strings.Repeat("x", scion.TaskArgvBudget+1)})

	rec := callWorker(t, b, "/worker/start", string(body), "test-manager")

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "instructions_file") {
		t.Fatalf("the refusal must point at the fix; body=%q", rec.Body.String())
	}
	if len(rt.started) != 0 {
		t.Fatalf("no start may be attempted for an oversized task; got %d", len(rt.started))
	}
}

// A worker manual over scion's hub-request cap is an operator error: 500 with
// nothing staged, the same shape as an unreadable file. The manager sees the
// generic body; the reason and the path stay in the host-side audit line.
func TestWorkerStart_absent_oversizedInstructionsFailBeforeStaging(t *testing.T) {
	dir := t.TempDir()
	manual := filepath.Join(dir, "worker-manual.md")
	if err := os.WriteFile(manual, []byte(strings.Repeat("m", scion.MaxInstructionsBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", HostWorkspace: filepath.Join(t.TempDir(), "workers", "worker"),
		BootstrapDir: filepath.Join(dir, ".lever"), Image: "img:1", InstructionsPath: manual}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}} // absent
	b := newTestBroker(t, rt, spec)

	rec := callWorker(t, b, "/worker/start", `{"worker":"worker","task":"do it"}`, "test-manager")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", rec.Code, rec.Body.String())
	}
	if len(rt.started) != 0 {
		t.Fatalf("no start may happen; got %d", len(rt.started))
	}
	if _, err := os.Stat(filepath.Join(spec.BootstrapDir, "bootstrap.json")); !os.IsNotExist(err) {
		t.Fatalf("no ticket may be staged (stat err %v)", err)
	}
}

// lever#31 on the worker path: the record is live when the broker first looks
// and dead a moment later. With a settle window the dispatch fails and the
// manager hears WHY — "came up, then died" with the observation — instead of a
// 200 "running" over an exited container.
func TestWorkerStartFailsWhenHarnessDiesDuringSettle(t *testing.T) {
	dir := t.TempDir()
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", HostWorkspace: filepath.Join(t.TempDir(), "workers", "worker"),
		BootstrapDir: filepath.Join(dir, ".lever"), Image: "img:1"}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}, dieAfterLists: 2}
	b := newTestBroker(t, rt, spec)
	b.liveAttempts, b.liveInterval, b.liveSettle = 3, time.Millisecond, 30*time.Millisecond

	rec := callWorker(t, b, "/worker/start", `{"worker":"worker","task":"do it"}`, "test-manager")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "then died") || !strings.Contains(rec.Body.String(), "Exited (1)") {
		t.Fatalf("body must say the worker came up then died, with the observation: %q", rec.Body.String())
	}
}

// Zero settle — what every broker built without brokerctl's production
// config gets — is the first-observation gate: the same death goes unseen.
// Pinned so the production default cannot be dropped without a test moving.
func TestWorkerStartZeroSettleMissesALaterDeath(t *testing.T) {
	dir := t.TempDir()
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", HostWorkspace: filepath.Join(t.TempDir(), "workers", "worker"),
		BootstrapDir: filepath.Join(dir, ".lever"), Image: "img:1"}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}, dieAfterLists: 1}
	b := newTestBroker(t, rt, spec)
	b.liveAttempts, b.liveInterval = 3, time.Millisecond

	if rec := callWorker(t, b, "/worker/start", `{"worker":"worker","task":"do it"}`, "test-manager"); rec.Code != http.StatusOK {
		t.Fatalf("zero settle returns on the first live look by design; got %d (%s)", rec.Code, rec.Body.String())
	}
}
