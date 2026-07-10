package broker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/scion"
)

// fakeRuntime records calls and returns scripted results; satisfies WorkerRuntime.
type fakeRuntime struct {
	agents       map[string][]scion.Agent // project -> agents (for List)
	started      []scion.StartOpts
	resumed      []string
	resumeProj   []string
	stopped      []string
	stopProj     []string
	suspend      []string
	suspendProj  []string
	envSets      []string
	envSetProj   []string
	startErr     error
	listCalls    int      // total List invocations, to assert the fan-out is collapsed
	listProjects []string // project arg of every List call
}

func (f *fakeRuntime) List(_ context.Context, project string) ([]scion.Agent, error) {
	f.listCalls++
	f.listProjects = append(f.listProjects, project)
	return f.agents[project], nil
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
	b := New(Config{
		Workers: []WorkerSpec{{Name: "worker", WorkspaceSubdir: "workers/worker"}},
	})
	if _, ok := b.workerSpec("worker"); !ok {
		t.Fatal("expected worker spec present")
	}
	if _, ok := b.workerSpec("nope"); ok {
		t.Fatal("expected absent spec to be missing")
	}
}

// fakeTLSWithCN returns a synthetic TLS connection state whose verified client
// cert CN is cn. Sufficient for RequireAgent in tests that don't need a real CA.
func fakeTLSWithCN(cn string) *tls.ConnectionState {
	return &tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{
			{
				{Subject: pkix.Name{CommonName: cn}},
			},
		},
	}
}

// caTicketStore returns a fresh, empty TicketStore for tests.
func caTicketStore(_ *testing.T) *ca.TicketStore {
	return ca.NewTicketStore()
}

// newTestBroker builds a Broker with a fake runtime, a real ticket store, and a
// temp bootstrap dir for the given single worker.
func newTestBroker(t *testing.T, rt WorkerRuntime, spec WorkerSpec) *Broker {
	t.Helper()
	return New(Config{
		Tickets:         caTicketStore(t),
		Registry:        registry.New(),
		Runtime:         rt,
		Workers:         []WorkerSpec{spec},
		BrokerCAPEM:     "CA-PEM",
		BrokerURL:       "https://10.0.0.2:8080",
		ManagerIdentity: "test-manager",
		InstanceProject: testInstanceProject,
	})
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
		BootstrapDir: filepath.Join(dir, ".lever"), Image: "img:1", APIKey: true}
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
	// --workspace subdir (no longer equal to each other) + api-key + image
	if len(rt.started) != 1 {
		t.Fatalf("start calls = %d, want 1", len(rt.started))
	}
	got := rt.started[0]
	if got.Project != testInstanceProject || got.WorkspaceSubdir != "workers/worker" ||
		got.Worker != "worker" || got.Image != "img:1" || !got.APIKey {
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
	if _, err := os.Stat(filepath.Join(spec.BootstrapDir, "bootstrap.json")); err == nil {
		t.Fatal("resume must NOT re-provision/stage a bootstrap")
	}
}

// TestWorkerStart_terminalPhase_resumesNoReprovision verifies that any non-empty,
// non-running phase (e.g. "exited") takes the resume path and never mints a
// ticket, stages a bootstrap, or calls scion start.
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
	// must NOT have staged a bootstrap
	if _, err := os.Stat(filepath.Join(bootstrapDir, "bootstrap.json")); err == nil {
		t.Fatal("terminal-phase resume must NOT stage a bootstrap")
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
	b := New(Config{
		Tickets:  caTicketStore(t),
		Registry: registry.New(),
		Runtime:  rt,
		Workers: []WorkerSpec{
			{Name: "worker", WorkspaceSubdir: "workers/worker", BootstrapDir: t.TempDir()},
			{Name: "helper", WorkspaceSubdir: "workers/helper", BootstrapDir: t.TempDir()},
		},
		BrokerCAPEM:     "CA-PEM",
		BrokerURL:       "https://10.0.0.2:8080",
		ManagerIdentity: "test-manager",
		InstanceProject: testInstanceProject,
	})
	req := httptest.NewRequest("GET", "/worker/list", nil)
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
	req2 := httptest.NewRequest("GET", "/worker/list", nil)
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
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", BootstrapDir: t.TempDir()}
	// Build a broker with an explicit nil runtime (no LEVER_JAIL_USER/UID env).
	b := New(Config{
		Tickets:         caTicketStore(t),
		Registry:        registry.New(),
		Runtime:         nil, // unwired: simulates manual `lever broker serve`
		Workers:         []WorkerSpec{spec},
		BrokerCAPEM:     "CA-PEM",
		BrokerURL:       "https://10.0.0.2:8080",
		ManagerIdentity: "test-manager",
	})

	// /worker/start with manager CN must return 502, not panic.
	rec := callWorker(t, b, "/worker/start", `{"worker":"worker","task":"go"}`, "test-manager")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("/worker/start nil-runtime: status = %d, want 502", rec.Code)
	}

	// /worker/list with manager CN must also return 502, not panic.
	req := httptest.NewRequest("GET", "/worker/list", nil)
	req.TLS = fakeTLSWithCN("test-manager")
	rec2 := httptest.NewRecorder()
	b.JailHandler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusBadGateway {
		t.Fatalf("/worker/list nil-runtime: status = %d, want 502", rec2.Code)
	}
}

// TestWorkerNilRuntime_authzPrecedence proves that even with nil runtime, an
// unauthenticated or non-manager caller gets 403 (authz runs before the nil check).
func TestWorkerNilRuntime_authzPrecedence(t *testing.T) {
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker", BootstrapDir: t.TempDir()}
	b := New(Config{
		Tickets:         caTicketStore(t),
		Registry:        registry.New(),
		Runtime:         nil,
		Workers:         []WorkerSpec{spec},
		BrokerCAPEM:     "CA-PEM",
		BrokerURL:       "https://10.0.0.2:8080",
		ManagerIdentity: "test-manager",
	})

	// Non-manager CN on /worker/start must get 403, not 502.
	rec := callWorker(t, b, "/worker/start", `{"worker":"worker"}`, "intruder")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("/worker/start intruder nil-runtime: status = %d, want 403", rec.Code)
	}

	// Non-manager CN on /worker/list must get 403, not 502.
	req := httptest.NewRequest("GET", "/worker/list", nil)
	req.TLS = fakeTLSWithCN("intruder")
	rec2 := httptest.NewRecorder()
	b.JailHandler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("/worker/list intruder nil-runtime: status = %d, want 403", rec2.Code)
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
	if _, err := os.Stat(filepath.Join(spec.BootstrapDir, "bootstrap.json")); !os.IsNotExist(err) {
		t.Fatal("revoked dispatch must not stage bootstrap")
	}
}
