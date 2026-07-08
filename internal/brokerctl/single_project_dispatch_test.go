package brokerctl

// single_project_dispatch_test.go is Task 6's broker-side single-project
// integration proof (P2 plan). It drives the REAL config → WorkerSpecs →
// broker.Config → broker.New chain (BuildBroker + WorkerSpecs, exactly as
// Serve assembles them, minus the real backend/jail wiring) for a config with
// TWO workers (dir: workers/a, dir: workers/b), with only the scion runtime
// faked (no live VM). It proves the collapsed single-project dispatch shape:
// both workers start as agents within the SAME instance project (`-g`), each
// gets its OWN --workspace subdir, the host workspace dirs are actually
// created, and `list` fans in to exactly one runtime call returning the whole
// fleet — never one call per worker. Under the OLD per-worker-project model
// this test would fail three ways at once: the two starts' Project fields
// would differ (or equal their own Workspace) instead of sharing one constant,
// and the list assertion would see more than one runtime call.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/scion"
)

// fakeWorkerRuntime is a minimal broker.WorkerRuntime fake local to this
// package (broker's own fakeRuntime, in internal/broker/worker_test.go, is
// unexported and lives in a _test.go file, so it cannot be imported from
// here — importing brokerctl FROM internal/broker's test files would also be
// an import cycle, since brokerctl imports broker). Records every List/Start
// call so the test can assert call counts and the exact opts passed.
type fakeWorkerRuntime struct {
	agents       map[string][]scion.Agent // project -> agents, for List
	started      []scion.StartOpts
	listCalls    int
	listProjects []string
}

func (f *fakeWorkerRuntime) List(_ context.Context, project string) ([]scion.Agent, error) {
	f.listCalls++
	f.listProjects = append(f.listProjects, project)
	return f.agents[project], nil
}
func (f *fakeWorkerRuntime) Start(_ context.Context, o scion.StartOpts) error {
	f.started = append(f.started, o)
	return nil
}
func (f *fakeWorkerRuntime) Resume(_ context.Context, _, _ string) error  { return nil }
func (f *fakeWorkerRuntime) Stop(_ context.Context, _, _ string) error    { return nil }
func (f *fakeWorkerRuntime) Suspend(_ context.Context, _, _ string) error { return nil }
func (f *fakeWorkerRuntime) EnvSet(_ context.Context, _, _, _ string) error {
	return nil
}
func (f *fakeWorkerRuntime) Message(_ context.Context, _ scion.MsgOpts) error { return nil }
func (f *fakeWorkerRuntime) Inbox(_ context.Context, _ bool, _ string) ([]scion.Event, error) {
	return nil, nil
}

// fakeTLSWithCN returns a synthetic TLS connection state whose verified client
// cert CN is cn — enough for ca.RequireAgent (it reads
// r.TLS.VerifiedChains[0][0].Subject.CommonName), without a real handshake.
// Mirrors internal/broker/worker_test.go's helper of the same shape (kept
// local for the same reason as fakeWorkerRuntime above).
func fakeTLSWithCN(cn string) *tls.ConnectionState {
	return &tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{
			{{Subject: pkix.Name{CommonName: cn}}},
		},
	}
}

// TestSingleProjectWorkerDispatchAndList drives the collapsed single-project
// broker dispatch end to end (fake scion runtime only) for a two-worker
// config, per the P2 plan's Task 6.
func TestSingleProjectWorkerDispatchAndList(t *testing.T) {
	tree := t.TempDir() // real dir: HostWorkspace MkdirAll is genuine, assertable
	kp, err := token.Generate()
	if err != nil {
		t.Fatalf("token.Generate: %v", err)
	}
	caInst, err := ca.Generate()
	if err != nil {
		t.Fatalf("ca.Generate: %v", err)
	}

	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
		Broker:  config.Broker{LLMAuth: config.LLMAuthSubscription},
		Workers: []config.Worker{
			{Name: "a", Dir: "workers/a"},
			{Name: "b", Dir: "workers/b"},
		},
	}

	// Config-derived, path-authoritative worker specs — the SAME production
	// function Serve calls (see serve.go: cfg.Workers = WorkerSpecs(app, jailMount)).
	const jailMount = "/lever"
	specs := WorkerSpecs(app, jailMount)
	if len(specs) != 2 {
		t.Fatalf("WorkerSpecs = %d specs, want 2", len(specs))
	}

	rt := &fakeWorkerRuntime{agents: map[string][]scion.Agent{}} // both workers absent
	cfg, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		t.Fatalf("BuildBroker: %v", err)
	}
	// The remaining fields mirror Serve's non-backend-dependent wiring (serve.go):
	// only Runtime is faked — everything else is the real production assembly.
	cfg.Runtime = rt
	cfg.Workers = specs
	cfg.InstanceProject = jailMount
	cfg.ManagerSlug = app.Name
	cfg.BrokerCAPEM = "CA-PEM"
	cfg.BrokerURL = "https://10.0.0.2:8080"
	b := broker.New(cfg)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.TLS = fakeTLSWithCN(cfg.ManagerIdentity)
		rec := httptest.NewRecorder()
		b.JailHandler().ServeHTTP(rec, r)
		return rec
	}

	// --- Point: worker A and worker B start, both under the SAME instance
	// project, each with its OWN --workspace subdir. ---
	recA := call("POST", "/worker/start", `{"worker":"a","task":"do a"}`)
	if recA.Code != http.StatusOK {
		t.Fatalf("worker a start status = %d (%s)", recA.Code, recA.Body.String())
	}
	recB := call("POST", "/worker/start", `{"worker":"b","task":"do b"}`)
	if recB.Code != http.StatusOK {
		t.Fatalf("worker b start status = %d (%s)", recB.Code, recB.Body.String())
	}

	if len(rt.started) != 2 {
		t.Fatalf("Start calls = %d, want 2", len(rt.started))
	}
	optsA, optsB := rt.started[0], rt.started[1]
	if optsA.Project != jailMount || optsA.Workspace != "/lever/workers/a" {
		t.Fatalf("worker a StartOpts = %+v, want Project=%q Workspace=/lever/workers/a", optsA, jailMount)
	}
	if optsB.Project != jailMount || optsB.Workspace != "/lever/workers/b" {
		t.Fatalf("worker b StartOpts = %+v, want Project=%q Workspace=/lever/workers/b", optsB, jailMount)
	}
	if optsA.Project != optsB.Project {
		t.Fatalf("both workers must share the SAME instance project (-g): a=%q b=%q", optsA.Project, optsB.Project)
	}
	if optsA.Workspace == optsB.Workspace {
		t.Fatalf("each worker must get its OWN --workspace subdir, got the same for both: %q", optsA.Workspace)
	}

	// --- Point: each worker's HostWorkspace dir was actually created on
	// dispatch (real filesystem, t.TempDir()-backed tree). ---
	for _, s := range specs {
		fi, err := os.Stat(s.HostWorkspace)
		if err != nil {
			t.Fatalf("worker %q HostWorkspace %q not created: %v", s.Name, s.HostWorkspace, err)
		}
		if !fi.IsDir() {
			t.Fatalf("worker %q HostWorkspace %q is not a directory", s.Name, s.HostWorkspace)
		}
	}

	// --- Point: /worker/list fans in to exactly ONE runtime List call with
	// the instance project, returning BOTH agents — never one call per worker. ---
	rt.agents[jailMount] = []scion.Agent{
		{Slug: "a", Phase: "running"},
		{Slug: "b", Phase: "running"},
	}
	listCallsBefore := rt.listCalls // the two /worker/start calls above each phase-check via List
	rec := call("GET", "/worker/list", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d (%s)", rec.Code, rec.Body.String())
	}
	if delta := rt.listCalls - listCallsBefore; delta != 1 {
		t.Fatalf("List calls for /worker/list = %d, want exactly 1 (collapsed fan-out)", delta)
	}
	if got := rt.listProjects[len(rt.listProjects)-1]; got != jailMount {
		t.Fatalf("List project = %q, want the instance project %q", got, jailMount)
	}
	var out struct {
		Agents []scion.Agent `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode /worker/list response: %v", err)
	}
	if len(out.Agents) != 2 {
		t.Fatalf("list returned %d agents, want 2 (both workers from the ONE call)", len(out.Agents))
	}
}
