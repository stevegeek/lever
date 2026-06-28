package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/agent"
	"github.com/lever-to/lever/internal/config"
	"github.com/lever-to/lever/internal/exec"
	"github.com/lever-to/lever/internal/scion"
)

func clientWith(f *exec.FakeRunner) ClientFactory {
	return func() *scion.Client {
		return scion.New(f, scion.Options{Bin: "scion", HubEndpoint: "http://127.0.0.1:8080"})
	}
}

func TestAgentStart(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	root := newManagerRootWith(clientWith(f))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"agent", "start", "appa", "--project", "/g/appa", "--image", "img:1", "--task", "do x"})
	if err := root.Execute(); err != nil {
		t.Fatalf("start: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if !strings.Contains(got, "start appa do x") || !strings.Contains(got, "-g /g/appa") {
		t.Fatalf("argv=%q", got)
	}
}

func TestAgentListPrints(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion list --format json -g /g/appa", exec.Result{Stdout: `[{"slug":"appa","phase":"running"}]`})
	root := newManagerRootWith(clientWith(f))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"agent", "list", "--project", "/g/appa"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), "appa") || !strings.Contains(out.String(), "running") {
		t.Fatalf("out=%q", out.String())
	}
}

func TestAgentRegister(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion init", exec.Result{})
	f.Script("scion hub link", exec.Result{})
	root := newManagerRootWith(clientWith(f))
	root.SetArgs([]string{"agent", "register", "/g/appa"})
	if err := root.Execute(); err != nil {
		t.Fatalf("register: %v", err)
	}
	if f.Calls[0].Dir != "/g/appa" || f.Calls[0].Args[0] != "init" {
		t.Fatalf("init call=%+v", f.Calls[0])
	}
	if f.Calls[1].Args[0] != "hub" {
		t.Fatalf("hub link call=%+v", f.Calls[1])
	}
}

func TestAgentStartResolvesImageFromManifest(t *testing.T) {
	orig := loadManifest
	loadManifest = func(path string) (*config.Manifest, error) {
		return &config.Manifest{Groves: []config.ManifestGrove{
			{Name: "scratch", Image: "scionlocal/lever-claude:latest"},
			{Name: "rust", Image: "scionlocal/lever-rust:latest"},
		}}, nil
	}
	defer func() { loadManifest = orig }()

	cases := []struct {
		name, grove, wantImage string
	}{
		{"inherited image", "scratch", "scionlocal/lever-claude:latest"},
		{"override image", "rust", "scionlocal/lever-rust:latest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := exec.NewFakeRunner()
			f.Script("scion", exec.Result{})
			root := newManagerRootWith(clientWith(f))
			root.SetArgs([]string{"agent", "start", tc.grove, "--manifest", "/x/.lever-manifest.yaml", "-g", "groves/" + tc.grove})
			if err := root.Execute(); err != nil {
				t.Fatalf("start: %v", err)
			}
			got := strings.Join(f.Calls[0].Args, " ")
			if !strings.Contains(got, "--image "+tc.wantImage) {
				t.Fatalf("argv=%q want --image %q", got, tc.wantImage)
			}
		})
	}
}

func TestAgentStartExplicitImageWinsOverManifest(t *testing.T) {
	orig := loadManifest
	loadManifest = func(path string) (*config.Manifest, error) {
		return &config.Manifest{Groves: []config.ManifestGrove{{Name: "scratch", Image: "from-manifest:1"}}}, nil
	}
	defer func() { loadManifest = orig }()

	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	root := newManagerRootWith(clientWith(f))
	root.SetArgs([]string{"agent", "start", "scratch", "--manifest", "/x/.lever-manifest.yaml", "--image", "explicit:9"})
	if err := root.Execute(); err != nil {
		t.Fatalf("start: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if !strings.Contains(got, "--image explicit:9") || strings.Contains(got, "from-manifest:1") {
		t.Fatalf("argv=%q want explicit image to win", got)
	}
}

func TestAgentStartUnknownGroveOmitsImage(t *testing.T) {
	orig := loadManifest
	loadManifest = func(path string) (*config.Manifest, error) {
		return &config.Manifest{Groves: []config.ManifestGrove{{Name: "scratch", Image: "img:1"}}}, nil
	}
	defer func() { loadManifest = orig }()

	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	root := newManagerRootWith(clientWith(f))
	// grove not in the manifest, no --image → no --image passed (caller must specify)
	root.SetArgs([]string{"agent", "start", "ghost", "--manifest", "/x/.lever-manifest.yaml", "-g", "groves/ghost"})
	if err := root.Execute(); err != nil {
		t.Fatalf("start: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if strings.Contains(got, "--image") {
		t.Fatalf("argv=%q should omit --image for an unknown grove", got)
	}
}

func TestAgentStartNoManifestOmitsImage(t *testing.T) {
	t.Setenv("LEVER_MANIFEST", "")
	t.Chdir(t.TempDir()) // no manifest in cwd
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	root := newManagerRootWith(clientWith(f))
	root.SetArgs([]string{"agent", "start", "scratch", "-g", "groves/scratch"})
	if err := root.Execute(); err != nil {
		t.Fatalf("start: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if strings.Contains(got, "--image") {
		t.Fatalf("argv=%q should not contain --image without a manifest", got)
	}
}

func TestAgentStartDiscoversManifestForImage(t *testing.T) {
	t.Setenv("LEVER_MANIFEST", "")
	dir := t.TempDir()
	if err := config.WriteManifest(dir, config.Manifest{Groves: []config.ManifestGrove{{Name: "worker", Image: "img:1"}}}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	root := newManagerRootWith(clientWith(f))
	root.SetArgs([]string{"agent", "start", "worker", "-g", "groves/worker"})
	if err := root.Execute(); err != nil {
		t.Fatalf("start: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if !strings.Contains(got, "--image img:1") {
		t.Fatalf("argv=%q want --image img:1 (resolved from discovered manifest)", got)
	}
}

// TestAgentStartStagesGroveBootstrap asserts that agent start:
//  1. Calls the injected provisioner (before scion.Start).
//  2. Writes <groveProject>/.lever/bootstrap.json (0600, dir 0700) with the
//     correct agent_cn, ticket, broker_ca, and broker_url.
//  3. Only then invokes scion.Start (ordering proved by the intercepting runner).
func TestAgentStartStagesGroveBootstrap(t *testing.T) {
	// Arrange: a temp dir for the grove workspace.
	groveDir := t.TempDir()
	bootstrapPath := filepath.Join(groveDir, ".lever", "bootstrap.json")

	// Fake provisioner: returns a fixed Bootstrap for grove "worker".
	wantTicket := "tk-abc123"
	wantCA := "fake-ca-pem"
	wantURL := "https://broker.example:8443"
	var provisionCalled bool
	fakeProvision := func(_ context.Context, grove string) (agent.Bootstrap, error) {
		provisionCalled = true
		return agent.Bootstrap{
			Ticket:    wantTicket,
			BrokerCA:  wantCA,
			BrokerURL: wantURL,
			AgentCN:   grove,
		}, nil
	}

	// Inject provisioner via package-level seam.
	orig := provisionGrove
	provisionGrove = fakeProvision
	defer func() { provisionGrove = orig }()

	// Use an intercepting runner that checks bootstrap existence on scion start.
	var startCalled bool
	var bootstrapExistedBeforeStart bool
	interceptor := &interceptRunner{
		FakeRunner: exec.NewFakeRunner(),
		onStart: func() {
			startCalled = true
			_, err := os.Stat(bootstrapPath)
			bootstrapExistedBeforeStart = err == nil
		},
	}
	interceptor.Script("scion", exec.Result{})

	cf := func() *scion.Client {
		return scion.New(interceptor, scion.Options{Bin: "scion", HubEndpoint: "http://127.0.0.1:8080"})
	}
	root := newManagerRootWith(cf)
	root.SetArgs([]string{"agent", "start", "worker", "--project", groveDir, "--image", "img:1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Assert provisioner was called.
	if !provisionCalled {
		t.Fatal("fake provisioner was not called")
	}

	// Assert scion.Start was called.
	if !startCalled {
		t.Fatal("scion.Start was not called")
	}

	// Assert bootstrap was staged BEFORE Start.
	if !bootstrapExistedBeforeStart {
		t.Fatal("bootstrap.json did not exist when scion.Start was called (staging must happen first)")
	}

	// Assert bootstrap.json permissions (0600 file, 0700 dir).
	leverDir := filepath.Join(groveDir, ".lever")
	di, err := os.Stat(leverDir)
	if err != nil {
		t.Fatalf("stat .lever dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Fatalf(".lever dir perm = %04o, want 0700", perm)
	}
	fi, err := os.Stat(bootstrapPath)
	if err != nil {
		t.Fatalf("stat bootstrap.json: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("bootstrap.json perm = %04o, want 0600", perm)
	}

	// Assert bootstrap.json contents.
	raw, err := os.ReadFile(bootstrapPath)
	if err != nil {
		t.Fatalf("read bootstrap.json: %v", err)
	}
	var bs agent.Bootstrap
	if err := json.Unmarshal(raw, &bs); err != nil {
		t.Fatalf("unmarshal bootstrap.json: %v", err)
	}
	if bs.AgentCN != "worker" {
		t.Errorf("agent_cn = %q, want %q", bs.AgentCN, "worker")
	}
	if bs.Ticket != wantTicket {
		t.Errorf("ticket = %q, want %q", bs.Ticket, wantTicket)
	}
	if bs.BrokerCA != wantCA {
		t.Errorf("broker_ca = %q, want %q", bs.BrokerCA, wantCA)
	}
	if bs.BrokerURL != wantURL {
		t.Errorf("broker_url = %q, want %q", bs.BrokerURL, wantURL)
	}
}

// TestAgentStartNoBrokerSkipsBootstrap asserts that when the provisioner returns
// an empty Bootstrap (no broker configured), agent start: (1) writes NO
// .lever/bootstrap.json, (2) still invokes scion Start, (3) returns no error.
func TestAgentStartNoBrokerSkipsBootstrap(t *testing.T) {
	groveDir := t.TempDir()
	bootstrapPath := filepath.Join(groveDir, ".lever", "bootstrap.json")

	// Fake provisioner returning empty ticket (no broker configured).
	var provisionCalled bool
	fakeProvision := func(_ context.Context, _ string) (agent.Bootstrap, error) {
		provisionCalled = true
		return agent.Bootstrap{}, nil // empty ticket = no broker
	}

	orig := provisionGrove
	provisionGrove = fakeProvision
	defer func() { provisionGrove = orig }()

	var startCalled bool
	interceptor := &interceptRunner{
		FakeRunner: exec.NewFakeRunner(),
		onStart: func() {
			startCalled = true
		},
	}
	interceptor.Script("scion", exec.Result{})

	cf := func() *scion.Client {
		return scion.New(interceptor, scion.Options{Bin: "scion", HubEndpoint: "http://127.0.0.1:8080"})
	}
	root := newManagerRootWith(cf)
	root.SetArgs([]string{"agent", "start", "worker", "--project", groveDir, "--image", "img:1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("start: %v", err)
	}

	if !provisionCalled {
		t.Fatal("fake provisioner was not called")
	}
	if !startCalled {
		t.Fatal("scion.Start was not called")
	}
	if _, err := os.Stat(bootstrapPath); !os.IsNotExist(err) {
		t.Fatalf("bootstrap.json should not exist when no broker configured, got stat err: %v", err)
	}
}

// TestAgentStartConveysLLMAuthForAPIKeyGrove asserts that when the manifest
// marks a grove api-key, agent start issues a project-scoped
// `hub env set --project LEVER_LLM_AUTH=api-key` (in the grove's project dir)
// BEFORE scion start — mirroring the manager path so the grove's pre-start hook
// enters api-key mode. A subscription grove gets no such call.
func TestAgentStartConveysLLMAuthForAPIKeyGrove(t *testing.T) {
	orig := loadManifest
	loadManifest = func(string) (*config.Manifest, error) {
		return &config.Manifest{Groves: []config.ManifestGrove{
			{Name: "secure", Image: "img:1", LLMAuth: config.LLMAuthAPIKey},
			{Name: "open", Image: "img:1", LLMAuth: config.LLMAuthSubscription},
		}}, nil
	}
	defer func() { loadManifest = orig }()
	// Skip provisioning (no broker) so the only calls are env-set + start.
	op := provisionGrove
	provisionGrove = nil
	defer func() { provisionGrove = op }()

	t.Run("api-key grove gets env-set before start", func(t *testing.T) {
		f := exec.NewFakeRunner()
		f.Script("scion", exec.Result{})
		root := newManagerRootWith(clientWith(f))
		root.SetArgs([]string{"agent", "start", "secure", "--manifest", "/x/.lever-manifest.yaml", "-g", "/g/secure"})
		if err := root.Execute(); err != nil {
			t.Fatalf("start: %v", err)
		}
		envIdx, startIdx := -1, -1
		for i, c := range f.Calls {
			argv := strings.Join(c.Args, " ")
			if strings.Contains(argv, "hub env set --project LEVER_LLM_AUTH=api-key") && c.Dir == "/g/secure" {
				envIdx = i
			}
			if c.Name == "scion" && strings.Contains(argv, " start ") {
				startIdx = i
			}
		}
		if envIdx == -1 {
			t.Fatalf("no `hub env set --project LEVER_LLM_AUTH=api-key` in /g/secure; calls=%+v", f.Calls)
		}
		if startIdx == -1 || envIdx > startIdx {
			t.Fatalf("env-set (idx %d) must precede start (idx %d)", envIdx, startIdx)
		}
	})

	t.Run("subscription grove gets no env-set", func(t *testing.T) {
		f := exec.NewFakeRunner()
		f.Script("scion", exec.Result{})
		root := newManagerRootWith(clientWith(f))
		root.SetArgs([]string{"agent", "start", "open", "--manifest", "/x/.lever-manifest.yaml", "-g", "/g/open"})
		if err := root.Execute(); err != nil {
			t.Fatalf("start: %v", err)
		}
		for _, c := range f.Calls {
			if strings.Contains(strings.Join(c.Args, " "), "LEVER_LLM_AUTH") {
				t.Fatalf("subscription grove should not set LEVER_LLM_AUTH; got %+v", c)
			}
		}
	})
}

// interceptRunner wraps FakeRunner and calls onStart when it sees "scion start".
type interceptRunner struct {
	*exec.FakeRunner
	onStart func()
}

func (r *interceptRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (exec.Result, error) {
	if name == "scion" {
		for _, a := range args {
			if a == "start" {
				if r.onStart != nil {
					r.onStart()
				}
				break
			}
		}
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *interceptRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (exec.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}
