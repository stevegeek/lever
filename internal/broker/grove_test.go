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

// fakeRuntime records calls and returns scripted results; satisfies GroveRuntime.
type fakeRuntime struct {
	agents   map[string][]scion.Agent // project -> agents (for List)
	started  []scion.StartOpts
	resumed  []string
	stopped  []string
	suspend  []string
	envSets  []string
	startErr error
}

func (f *fakeRuntime) List(_ context.Context, project string) ([]scion.Agent, error) {
	return f.agents[project], nil
}
func (f *fakeRuntime) Start(_ context.Context, o scion.StartOpts) error {
	f.started = append(f.started, o)
	return f.startErr
}
func (f *fakeRuntime) Resume(_ context.Context, grove, _ string) error {
	f.resumed = append(f.resumed, grove)
	return nil
}
func (f *fakeRuntime) Stop(_ context.Context, grove, _ string) error {
	f.stopped = append(f.stopped, grove)
	return nil
}
func (f *fakeRuntime) Suspend(_ context.Context, grove, _ string) error {
	f.suspend = append(f.suspend, grove)
	return nil
}
func (f *fakeRuntime) EnvSet(_ context.Context, _, _, _ string) error {
	f.envSets = append(f.envSets, "set")
	return nil
}
func (f *fakeRuntime) Message(_ context.Context, _ scion.MsgOpts) error { return nil }
func (f *fakeRuntime) Inbox(_ context.Context, _ bool, _ string) ([]scion.Event, error) {
	return nil, nil
}

func TestGroveSpecLookup(t *testing.T) {
	b := New(Config{
		Groves: []GroveSpec{{Name: "worker", JailProject: "/lever/groves/worker"}},
	})
	if _, ok := b.groveSpec("worker"); !ok {
		t.Fatal("expected worker spec present")
	}
	if _, ok := b.groveSpec("nope"); ok {
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
// temp bootstrap dir for the given single grove.
func newTestBroker(t *testing.T, rt GroveRuntime, spec GroveSpec) *Broker {
	t.Helper()
	return New(Config{
		Tickets:         caTicketStore(t),
		Registry:        registry.New(),
		Runtime:         rt,
		Groves:          []GroveSpec{spec},
		BrokerCAPEM:     "CA-PEM",
		BrokerURL:       "https://10.0.0.2:8080",
		ManagerIdentity: "test-manager",
	})
}

// callGrove drives a handler with a synthetic verified client cert CN.
func callGrove(t *testing.T, b *Broker, path, body, cn string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.TLS = fakeTLSWithCN(cn)
	rec := httptest.NewRecorder()
	b.JailHandler().ServeHTTP(rec, req)
	return rec
}

func TestGroveStart_absent_provisionsStagesStarts(t *testing.T) {
	dir := t.TempDir()
	spec := GroveSpec{Name: "worker", JailProject: "/lever/groves/worker",
		BootstrapDir: filepath.Join(dir, ".lever"), Image: "img:1", APIKey: true}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}} // absent
	b := newTestBroker(t, rt, spec)

	rec := callGrove(t, b, "/grove/start", `{"grove":"worker","task":"do it"}`, "test-manager")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	// bootstrap staged 0600 with the broker CA/URL and the grove CN
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
	// scion start called with jail-absolute -g/--workspace + api-key + image
	if len(rt.started) != 1 {
		t.Fatalf("start calls = %d, want 1", len(rt.started))
	}
	got := rt.started[0]
	if got.Project != "/lever/groves/worker" || got.Workspace != "/lever/groves/worker" ||
		got.Grove != "worker" || got.Image != "img:1" || !got.APIKey {
		t.Fatalf("bad StartOpts: %+v", got)
	}
	if got.Task != "do it" {
		t.Fatalf("StartOpts.Task = %q, want \"do it\"", got.Task)
	}
	if len(rt.envSets) != 1 {
		t.Fatalf("EnvSet calls = %d, want 1 (api-key)", len(rt.envSets))
	}
}

func TestGroveStart_running_isNoop(t *testing.T) {
	spec := GroveSpec{Name: "worker", JailProject: "/lever/groves/worker", BootstrapDir: t.TempDir()}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{
		"/lever/groves/worker": {{Slug: "worker", Phase: "running"}},
	}}
	b := newTestBroker(t, rt, spec)
	rec := callGrove(t, b, "/grove/start", `{"grove":"worker"}`, "test-manager")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(rt.started) != 0 || len(rt.resumed) != 0 {
		t.Fatal("running grove must be a no-op")
	}
}

func TestGroveStart_suspended_resumesNoReprovision(t *testing.T) {
	spec := GroveSpec{Name: "worker", JailProject: "/lever/groves/worker",
		BootstrapDir: filepath.Join(t.TempDir(), ".lever")}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{
		"/lever/groves/worker": {{Slug: "worker", Phase: "suspended"}},
	}}
	b := newTestBroker(t, rt, spec)
	rec := callGrove(t, b, "/grove/start", `{"grove":"worker"}`, "test-manager")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(rt.resumed) != 1 || len(rt.started) != 0 {
		t.Fatalf("expected resume-only; resumed=%d started=%d", len(rt.resumed), len(rt.started))
	}
	if _, err := os.Stat(filepath.Join(spec.BootstrapDir, "bootstrap.json")); err == nil {
		t.Fatal("resume must NOT re-provision/stage a bootstrap")
	}
}

// TestGroveStart_terminalPhase_resumesNoReprovision verifies that any non-empty,
// non-running phase (e.g. "exited") takes the resume path and never mints a
// ticket, stages a bootstrap, or calls scion start.
func TestGroveStart_terminalPhase_resumesNoReprovision(t *testing.T) {
	bootstrapDir := filepath.Join(t.TempDir(), ".lever")
	spec := GroveSpec{Name: "worker", JailProject: "/lever/groves/worker",
		BootstrapDir: bootstrapDir}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{
		"/lever/groves/worker": {{Slug: "worker", Phase: "exited"}},
	}}
	b := newTestBroker(t, rt, spec)
	rec := callGrove(t, b, "/grove/start", `{"grove":"worker"}`, "test-manager")
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

func TestGroveStart_authz(t *testing.T) {
	spec := GroveSpec{Name: "worker", JailProject: "/lever/groves/worker", BootstrapDir: t.TempDir()}
	b := newTestBroker(t, &fakeRuntime{agents: map[string][]scion.Agent{}}, spec)
	// wrong CN
	if rec := callGrove(t, b, "/grove/start", `{"grove":"worker"}`, "intruder"); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong-CN status = %d, want 403", rec.Code)
	}
	// undeclared grove
	if rec := callGrove(t, b, "/grove/start", `{"grove":"ghost"}`, "test-manager"); rec.Code != http.StatusForbidden {
		t.Fatalf("undeclared grove status = %d, want 403", rec.Code)
	}
}

func TestGroveList(t *testing.T) {
	spec := GroveSpec{Name: "worker", JailProject: "/lever/groves/worker", BootstrapDir: t.TempDir()}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{
		"/lever/groves/worker": {{Slug: "worker", Phase: "running", Activity: "building"}},
	}}
	b := newTestBroker(t, rt, spec)
	req := httptest.NewRequest("GET", "/grove/list", nil)
	req.TLS = fakeTLSWithCN("test-manager")
	rec := httptest.NewRecorder()
	b.JailHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Agents []scion.Agent `json:"agents"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Agents) != 1 || out.Agents[0].Slug != "worker" || out.Agents[0].Phase != "running" {
		t.Fatalf("bad list: %+v", out.Agents)
	}
	// non-manager rejected
	req2 := httptest.NewRequest("GET", "/grove/list", nil)
	req2.TLS = fakeTLSWithCN("intruder")
	rec2 := httptest.NewRecorder()
	b.JailHandler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("intruder list = %d, want 403", rec2.Code)
	}
}

// TestGroveNilRuntime_returns502 proves that when the scion runtime is unwired
// (nil) the grove handlers return 502, not a panic from a nil-interface call.
func TestGroveNilRuntime_returns502(t *testing.T) {
	spec := GroveSpec{Name: "worker", JailProject: "/lever/groves/worker", BootstrapDir: t.TempDir()}
	// Build a broker with an explicit nil runtime (no LEVER_JAIL_USER/UID env).
	b := New(Config{
		Tickets:         caTicketStore(t),
		Registry:        registry.New(),
		Runtime:         nil, // unwired: simulates manual `lever broker serve`
		Groves:          []GroveSpec{spec},
		BrokerCAPEM:     "CA-PEM",
		BrokerURL:       "https://10.0.0.2:8080",
		ManagerIdentity: "test-manager",
	})

	// /grove/start with manager CN must return 502, not panic.
	rec := callGrove(t, b, "/grove/start", `{"grove":"worker","task":"go"}`, "test-manager")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("/grove/start nil-runtime: status = %d, want 502", rec.Code)
	}

	// /grove/list with manager CN must also return 502, not panic.
	req := httptest.NewRequest("GET", "/grove/list", nil)
	req.TLS = fakeTLSWithCN("test-manager")
	rec2 := httptest.NewRecorder()
	b.JailHandler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusBadGateway {
		t.Fatalf("/grove/list nil-runtime: status = %d, want 502", rec2.Code)
	}
}

// TestGroveNilRuntime_authzPrecedence proves that even with nil runtime, an
// unauthenticated or non-manager caller gets 403 (authz runs before the nil check).
func TestGroveNilRuntime_authzPrecedence(t *testing.T) {
	spec := GroveSpec{Name: "worker", JailProject: "/lever/groves/worker", BootstrapDir: t.TempDir()}
	b := New(Config{
		Tickets:         caTicketStore(t),
		Registry:        registry.New(),
		Runtime:         nil,
		Groves:          []GroveSpec{spec},
		BrokerCAPEM:     "CA-PEM",
		BrokerURL:       "https://10.0.0.2:8080",
		ManagerIdentity: "test-manager",
	})

	// Non-manager CN on /grove/start must get 403, not 502.
	rec := callGrove(t, b, "/grove/start", `{"grove":"worker"}`, "intruder")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("/grove/start intruder nil-runtime: status = %d, want 403", rec.Code)
	}

	// Non-manager CN on /grove/list must get 403, not 502.
	req := httptest.NewRequest("GET", "/grove/list", nil)
	req.TLS = fakeTLSWithCN("intruder")
	rec2 := httptest.NewRecorder()
	b.JailHandler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("/grove/list intruder nil-runtime: status = %d, want 403", rec2.Code)
	}
}

func TestGroveLifecycleVerbs(t *testing.T) {
	spec := GroveSpec{Name: "worker", JailProject: "/lever/groves/worker", BootstrapDir: t.TempDir()}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}}
	b := newTestBroker(t, rt, spec)

	for _, tc := range []struct {
		path  string
		check func() bool
	}{
		{"/grove/stop", func() bool { return len(rt.stopped) == 1 }},
		{"/grove/suspend", func() bool { return len(rt.suspend) == 1 }},
		{"/grove/resume", func() bool { return len(rt.resumed) == 1 }},
	} {
		rec := callGrove(t, b, tc.path, `{"grove":"worker"}`, "test-manager")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", tc.path, rec.Code)
		}
		if !tc.check() {
			t.Fatalf("%s did not dispatch the scion call", tc.path)
		}
	}
	// authz still enforced
	if rec := callGrove(t, b, "/grove/stop", `{"grove":"ghost"}`, "test-manager"); rec.Code != http.StatusForbidden {
		t.Fatalf("undeclared stop = %d, want 403", rec.Code)
	}
}

func TestGroveStart_deniesRevokedManager(t *testing.T) {
	dir := t.TempDir()
	spec := GroveSpec{Name: "worker", JailProject: "/lever/groves/worker",
		BootstrapDir: filepath.Join(dir, ".lever"), Image: "img:1", APIKey: true}
	rt := &fakeRuntime{agents: map[string][]scion.Agent{}}
	b := newTestBroker(t, rt, spec)
	b.Revoke("test-manager")

	rec := callGrove(t, b, "/grove/start", `{"grove":"worker","task":"do it"}`, "test-manager")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoked manager dispatch: status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	// No bootstrap staged, no start attempted.
	if _, err := os.Stat(filepath.Join(spec.BootstrapDir, "bootstrap.json")); !os.IsNotExist(err) {
		t.Fatal("revoked dispatch must not stage bootstrap")
	}
}
