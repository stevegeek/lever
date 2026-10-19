package brokerctl

// The broker-side single-project dispatch proof: the real config ->
// WorkerSpecs -> BuildBroker -> broker.New chain for two workers, with only
// the scion runtime faked. Both workers start in the same instance project,
// each with its own --workspace subdir, the host workspace dirs are created,
// and `list` is exactly one runtime call for the whole fleet.

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

	"github.com/stevegeek/lever/internal/backend/registry"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
)

// fakeWorkerRuntime is local because broker's own fake lives in its _test
// files (and importing brokerctl there would be a cycle). It records every
// List/Start call.
type fakeWorkerRuntime struct {
	agents       map[string][]scion.Agent // project -> agents, for List
	started      []scion.StartOpts
	resumed      []string
	listCalls    int
	listProjects []string
	staged       map[string]int // worker -> envelopes staged through the guest channel
}

func (f *fakeWorkerRuntime) StageWorkerTicket(_ context.Context, worker string, _ []byte) error {
	if f.staged == nil {
		f.staged = map[string]int{}
	}
	f.staged[worker]++
	return nil
}

func (f *fakeWorkerRuntime) List(_ context.Context, project string) ([]scion.Agent, error) {
	f.listCalls++
	f.listProjects = append(f.listProjects, project)
	// A worker that was started or resumed reports running with a live
	// container, so the broker's waitWorkerLive gate passes. Seeded agents
	// take precedence.
	agents := append([]scion.Agent(nil), f.agents[project]...)
	for _, w := range f.acted() {
		if !hasSlug(agents, w) {
			agents = append(agents, scion.Agent{Slug: w, Phase: "running", ContainerStatus: "Up 1 second"})
		}
	}
	return agents, nil
}

func (f *fakeWorkerRuntime) acted() []string {
	acted := make([]string, 0, len(f.started)+len(f.resumed))
	for _, o := range f.started {
		acted = append(acted, o.Worker)
	}
	acted = append(acted, f.resumed...)
	return acted
}

func hasSlug(agents []scion.Agent, slug string) bool {
	for _, a := range agents {
		if a.Slug == slug {
			return true
		}
	}
	return false
}

func (f *fakeWorkerRuntime) Start(_ context.Context, o scion.StartOpts) error {
	f.started = append(f.started, o)
	return nil
}
func (f *fakeWorkerRuntime) Resume(_ context.Context, worker, _ string) error {
	f.resumed = append(f.resumed, worker)
	return nil
}

func (f *fakeWorkerRuntime) ResumeTask(ctx context.Context, worker, project, _ string) error {
	return f.Resume(ctx, worker, project)
}

func (f *fakeWorkerRuntime) ResumeForce(ctx context.Context, worker, project string) error {
	return f.Resume(ctx, worker, project)
}
func (f *fakeWorkerRuntime) Stop(_ context.Context, _, _ string) error    { return nil }
func (f *fakeWorkerRuntime) Suspend(_ context.Context, _, _ string) error { return nil }
func (f *fakeWorkerRuntime) EnvSet(_ context.Context, _, _, _ string) error {
	return nil
}
func (f *fakeWorkerRuntime) Message(_ context.Context, _ scion.MsgOpts) error { return nil }
func (f *fakeWorkerRuntime) Inbox(_ context.Context, _ bool, _ string) ([]scion.Event, error) {
	return nil, nil
}

// fakeTLSWithCN returns a TLS state whose client cert CN is cn, enough for
// ca.RequireAgent without a handshake.
func fakeTLSWithCN(cn string) *tls.ConnectionState {
	return &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{
			{Subject: pkix.Name{CommonName: cn}},
		},
	}
}

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

	// The same function Serve's dispatchConfig calls.
	const jailMount = "/lever"
	specs := WorkerSpecs(app, jailMount, "501")
	if len(specs) != 2 {
		t.Fatalf("WorkerSpecs = %d specs, want 2", len(specs))
	}

	rt := &fakeWorkerRuntime{agents: map[string][]scion.Agent{}} // both workers absent
	cfg, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		t.Fatalf("BuildBroker: %v", err)
	}
	// The real decorateConfig, so this proof cannot drift from Serve. With no
	// jail env it takes the no-Runtime path; the fake is injected after.
	be, err := registry.Select(app.Backend, proc.RealRunner{}, "lever-"+app.Name)
	if err != nil {
		t.Fatalf("registry.Select: %v", err)
	}
	st := state.ForConfig(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	if err := decorateConfig(&cfg, app, st, be, "test", ServeEnv{}); err != nil {
		t.Fatalf("decorateConfig: %v", err)
	}
	if cfg.Dispatch.InstanceProject != jailMount {
		t.Fatalf("decorateConfig InstanceProject = %q, want backend MountDest %q", cfg.Dispatch.InstanceProject, jailMount)
	}
	cfg.Dispatch.Runtime = rt
	// The no-env path also leaves tickets unwired and specs without a ticket
	// directory: inject both.
	cfg.Dispatch.Tickets = rt
	cfg.Dispatch.Workers = specs
	b := broker.New(cfg)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.TLS = fakeTLSWithCN(cfg.Identity.ManagerIdentity)
		rec := httptest.NewRecorder()
		b.JailHandler().ServeHTTP(rec, r)
		return rec
	}

	// Both workers start in the same instance project, each with its own
	// --workspace subdir.
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
	if optsA.Project != jailMount || optsA.WorkspaceSubdir != "workers/a" {
		t.Fatalf("worker a StartOpts = %+v, want Project=%q WorkspaceSubdir=workers/a", optsA, jailMount)
	}
	if optsB.Project != jailMount || optsB.WorkspaceSubdir != "workers/b" {
		t.Fatalf("worker b StartOpts = %+v, want Project=%q WorkspaceSubdir=workers/b", optsB, jailMount)
	}
	if optsA.Project != optsB.Project {
		t.Fatalf("both workers must share the SAME instance project (-g): a=%q b=%q", optsA.Project, optsB.Project)
	}
	if optsA.WorkspaceSubdir == optsB.WorkspaceSubdir {
		t.Fatalf("each worker must get its OWN workspace subdir, got the same for both: %q", optsA.WorkspaceSubdir)
	}

	// Each worker's ticket went to the guest channel under its name, and its
	// container mounts only its own ticket directory; nothing under the tree.
	if rt.staged["a"] != 1 || rt.staged["b"] != 1 {
		t.Fatalf("staged tickets = %v, want one each for a and b", rt.staged)
	}
	if len(optsA.Volumes) != 1 || optsA.Volumes[0].Source != "/run/user/501/lever/tickets/a" || !optsA.Volumes[0].ReadOnly {
		t.Fatalf("worker a volumes = %+v, want its own ticket dir read-only", optsA.Volumes)
	}
	if len(optsB.Volumes) != 1 || optsB.Volumes[0].Source != "/run/user/501/lever/tickets/b" {
		t.Fatalf("worker b volumes = %+v, want its own ticket dir", optsB.Volumes)
	}
	for _, s := range specs {
		if _, err := os.Stat(filepath.Join(s.HostWorkspace, ".lever")); !os.IsNotExist(err) {
			t.Fatalf("worker %q: a .lever directory exists under its tree subdir (err %v)", s.Name, err)
		}
	}

	// Each HostWorkspace dir was created on dispatch.
	for _, s := range specs {
		fi, err := os.Stat(s.HostWorkspace)
		if err != nil {
			t.Fatalf("worker %q HostWorkspace %q not created: %v", s.Name, s.HostWorkspace, err)
		}
		if !fi.IsDir() {
			t.Fatalf("worker %q HostWorkspace %q is not a directory", s.Name, s.HostWorkspace)
		}
	}

	// /worker/list is one runtime List call returning both agents.
	rt.agents[jailMount] = []scion.Agent{
		{Slug: "a", Phase: "running"},
		{Slug: "b", Phase: "running"},
	}
	listCallsBefore := rt.listCalls // the two /worker/start calls above each phase-check via List
	rec := call("POST", "/worker/list", "")
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
