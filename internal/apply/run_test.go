package apply

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lever-to/lever/internal/config"
	"github.com/lever-to/lever/internal/exec"
	"github.com/lever-to/lever/internal/scion"
)

// flakyStartRunner fails the first startFails `scion start` calls with the
// runtime-broker-unavailable error (the registration race), then defers to the
// wrapped FakeRunner. Used to prove start-manager retries.
type flakyStartRunner struct {
	*exec.FakeRunner
	startFails int
	startCalls int
}

func (r *flakyStartRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (exec.Result, error) {
	if name == "scion" {
		hasStart, hasServer := false, false
		for _, a := range args {
			if a == "start" {
				hasStart = true
			}
			if a == "server" {
				hasServer = true
			}
		}
		if hasStart && !hasServer { // agent start, not `scion server start`
			r.startCalls++
			if r.startCalls <= r.startFails {
				// Client.run builds its error from Stdout+Stderr, so the marker must
				// live there, not just in the Go error.
				return exec.Result{Code: 1, Stderr: "no_runtime_broker: No runtime brokers available for this project"}, fmt.Errorf("exit status 1")
			}
		}
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *flakyStartRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (exec.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// alreadyUpRunner simulates a fully-up instance on re-apply: `scion server
// start` and agent `start` return "already running"; everything else succeeds.
type alreadyUpRunner struct{ *exec.FakeRunner }

func (r *alreadyUpRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (exec.Result, error) {
	if name == "scion" {
		hasServer, hasStart := false, false
		for _, a := range args {
			if a == "server" {
				hasServer = true
			}
			if a == "start" {
				hasStart = true
			}
		}
		if hasServer && hasStart {
			return exec.Result{Code: 1, Stderr: "Error: server is already running (PID: 123)"}, fmt.Errorf("exit status 1")
		}
		if hasStart && !hasServer {
			return exec.Result{Code: 1, Stderr: "Error: agent already running"}, fmt.Errorf("exit status 1")
		}
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *alreadyUpRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (exec.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// TestRunIdempotentReapply: re-applying a fully-up instance is a clean no-op —
// an already-running scion server and manager are tolerated, and the mint step
// tolerates the broker's spent single-use /bootstrap latch (ErrBootstrapLatched).
func TestRunIdempotentReapply(t *testing.T) {
	tree := t.TempDir()
	// A prior apply already staged the manager's bootstrap ticket, so a spent
	// latch on this re-apply is tolerable (the manager has what it needs).
	if err := os.MkdirAll(filepath.Join(tree, ".lever"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, ".lever", "bootstrap.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &config.App{
		Name: "demo", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
	r := &alreadyUpRunner{FakeRunner: exec.NewFakeRunner()}
	r.Script("scion", exec.Result{Stdout: "ok"})
	mintCalled := false
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		JailMount: "/lever",
		// Same broker process as a prior apply ⇒ latch spent ⇒ ErrBootstrapLatched.
		// The mint step must tolerate it (the manager already has its bootstrap).
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			mintCalled = true
			return BootstrapMaterial{}, ErrBootstrapLatched
		},
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("re-apply of a fully-up instance must be a clean no-op: %v", err)
	}
	if !mintCalled {
		t.Fatal("mint must be CALLED (and tolerate the latch) — tied to the live broker, not a stale file")
	}
}

// TestRunMintBootstrapPropagatesRealError: a non-latch mint error (e.g. the
// broker is down) must NOT be swallowed.
func TestRunMintBootstrapPropagatesRealError(t *testing.T) {
	tree := t.TempDir()
	app := &config.App{Name: "demo", Backend: "orbstack", Tree: tree, Manager: config.Manager{Image: "img"}}
	r := &alreadyUpRunner{FakeRunner: exec.NewFakeRunner()}
	r.Script("scion", exec.Result{Stdout: "ok"})
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		JailMount: "/lever",
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			return BootstrapMaterial{}, fmt.Errorf("broker /bootstrap: connection refused")
		},
	}
	if err := Run(context.Background(), app, deps); err == nil {
		t.Fatal("a real mint error (not the latch) must propagate, not be tolerated")
	}
}

// TestRunLatchedWithoutStagedBootstrapFails: a spent latch with NO staged
// bootstrap ticket means a stale broker is being reused (its latch was consumed
// by an earlier run, but this tree has no ticket). The new manager could never
// enrol, so the mint step must fail loudly and point at `lever down`, rather than
// silently boot a doomed manager.
func TestRunLatchedWithoutStagedBootstrapFails(t *testing.T) {
	tree := t.TempDir() // nothing staged
	app := &config.App{Name: "demo", Backend: "orbstack", Tree: tree, Manager: config.Manager{Image: "img"}}
	r := &alreadyUpRunner{FakeRunner: exec.NewFakeRunner()}
	r.Script("scion", exec.Result{Stdout: "ok"})
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
		JailMount: "/lever",
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			return BootstrapMaterial{}, ErrBootstrapLatched
		},
	}
	err := Run(context.Background(), app, deps)
	if err == nil {
		t.Fatal("a spent latch with no staged bootstrap must fail loudly (stale broker)")
	}
	if !strings.Contains(err.Error(), "lever down") {
		t.Fatalf("error should guide the user to `lever down`, got: %v", err)
	}
}

func TestStartManagerRetriesOnBrokerUnavailable(t *testing.T) {
	// Make the retry fast for the test.
	origAtt, origInt := brokerStartAttempts, brokerStartInterval
	brokerStartAttempts, brokerStartInterval = 5, time.Millisecond
	defer func() { brokerStartAttempts, brokerStartInterval = origAtt, origInt }()

	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "workspace"), 0o755)
	cfg := filepath.Join(dir, config.CanonicalName)
	if err := os.WriteFile(cfg, []byte("name: hello\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\nmanager:\n  image: img\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := &flakyStartRunner{FakeRunner: exec.NewFakeRunner(), startFails: 2}
	r.Script("scion", exec.Result{Stdout: "ok"})
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(r, scion.Options{}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run should succeed after the broker race resolves: %v", err)
	}
	if r.startCalls != 3 {
		t.Fatalf("start attempted %d times, want 3 (2 transient failures + 1 success)", r.startCalls)
	}
}

func TestRunDispatchesStepsInOrder(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
		Manager: config.Manager{Image: "scionlocal/lever-claude:latest"},
		Groves:  []config.Grove{{Name: "worker", Dir: "groves/worker"}},
	}
	var jailUp, loadImg bool
	deps := Deps{
		JailUp: func(context.Context, *config.App) error { jailUp = true; return nil },
		LoadImage: func(_ context.Context, ref string) error {
			loadImg = (ref == "scionlocal/lever-claude:latest")
			return nil
		},
		Scion: scion.New(f, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !jailUp || !loadImg {
		t.Fatalf("host steps not called: jailUp=%v loadImg=%v", jailUp, loadImg)
	}
	j := ""
	for _, c := range f.Calls {
		j += strings.Join(c.Args, " ") + "|"
	}
	for _, want := range []string{"init --machine", "config set --global image_registry scionlocal", "server start", "init --non-interactive", "hub link", "start hello"} {
		if !strings.Contains(j, want) {
			t.Fatalf("missing scion call %q in: %q", want, j)
		}
	}
}

func TestRunCredentialStep(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: t.TempDir(),
		Manager: config.Manager{Image: "img", CredentialFile: "/x/token"},
	}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		ReadCred:  func(string) (string, error) { return "sk-ant-raw", nil },
		Scion:     scion.New(f, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	j := ""
	for _, c := range f.Calls {
		j += strings.Join(c.Args, " ") + "|"
	}
	// secret value is base64-encoded (scion >= da49e14): b64("sk-ant-raw")
	if want := "hub secret set CLAUDE_CODE_OAUTH_TOKEN c2stYW50LXJhdw=="; !strings.Contains(j, want) {
		t.Fatalf("missing scion call %q in: %q", want, j)
	}
}

func TestStartManagerPassesPrompt(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "workspace", "groves", "worker"), 0o755)
	// prompt lives at the instance ROOT (host-only), NOT under the mounted tree.
	if err := os.WriteFile(filepath.Join(dir, "manager.md"), []byte("Dispatch the worker grove to create HELLO."), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, config.CanonicalName)
	if err := os.WriteFile(cfg, []byte("name: hello\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\nmanager:\n  image: img\n  prompt_file: manager.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(f, scion.Options{}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawPrompt bool
	for _, c := range f.Calls {
		j := strings.Join(c.Args, " ")
		if strings.Contains(j, "start hello") && strings.Contains(j, "Dispatch the worker grove to create HELLO.") {
			sawPrompt = true
		}
	}
	if !sawPrompt {
		t.Fatalf("manager prompt not passed to start; calls=%+v", f.Calls)
	}
}

func TestStartManagerSetsLLMAuthEnvForAPIKey(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "workspace"), 0o755)
	keyPath := filepath.Join(dir, "api.key")
	if err := os.WriteFile(keyPath, []byte("sk-ant-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, config.CanonicalName)
	body := "name: hello\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: api-key\n  api_key_file: " + keyPath + "\nmanager:\n  image: img\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(f, scion.Options{}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawEnvSet, sawPlaceholder bool
	var startArgv string
	for _, c := range f.Calls {
		argv := strings.Join(c.Args, " ")
		if argv == "hub env set --project LEVER_LLM_AUTH=api-key" {
			sawEnvSet = true
		}
		// SecretSet base64-encodes the value, so match on the verb + key, not value.
		if len(c.Args) >= 4 && c.Args[0] == "hub" && c.Args[1] == "secret" && c.Args[2] == "set" && c.Args[3] == "ANTHROPIC_API_KEY" {
			sawPlaceholder = true
		}
		if c.Name == "scion" && strings.Contains(argv, " start ") {
			startArgv = argv
		}
	}
	if !sawEnvSet {
		t.Fatalf("api-key manager: expected LEVER_LLM_AUTH env set; calls=%+v", f.Calls)
	}
	if !sawPlaceholder {
		t.Fatalf("api-key manager: expected placeholder ANTHROPIC_API_KEY secret set; calls=%+v", f.Calls)
	}
	// api-key manager must start with --harness-auth api-key (not oauth-token).
	if !strings.Contains(startArgv, "--harness-auth api-key") || strings.Contains(startArgv, "oauth-token") {
		t.Fatalf("api-key manager start must use --harness-auth api-key (not oauth-token); argv=%q", startArgv)
	}
}

func TestStartManagerNoLLMAuthEnvForSubscription(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "workspace"), 0o755)
	cfg := filepath.Join(dir, config.CanonicalName)
	if err := os.WriteFile(cfg, []byte("name: hello\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\nmanager:\n  image: img\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		Scion:     scion.New(f, scion.Options{}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var startArgv string
	for _, c := range f.Calls {
		argv := strings.Join(c.Args, " ")
		if strings.Contains(argv, "LEVER_LLM_AUTH") {
			t.Fatalf("subscription manager must not set LEVER_LLM_AUTH; calls=%+v", f.Calls)
		}
		if c.Name == "scion" && strings.Contains(argv, " start ") {
			startArgv = argv
		}
	}
	// subscription manager keeps oauth-token auth (scion projects the OAuth token).
	if !strings.Contains(startArgv, "--harness-auth oauth-token") || strings.Contains(startArgv, "--no-auth") {
		t.Fatalf("subscription manager start must use oauth-token (not --no-auth); argv=%q", startArgv)
	}
}

func TestJailPathTranslation(t *testing.T) {
	cases := []struct {
		host, tree, mount, want string
	}{
		{"/tmp/foo", "/tmp/foo", "/lever", "/lever"},
		{"/tmp/foo/groves/worker", "/tmp/foo", "/lever", "/lever/groves/worker"},
		{"/tmp/foo", "/tmp/foo", "", "/tmp/foo"},
		{"/elsewhere", "/tmp/foo", "/lever", "/elsewhere"},
	}
	for _, c := range cases {
		if got := jailPath(c.host, c.tree, c.mount); got != c.want {
			t.Errorf("jailPath(%q, %q, %q) = %q, want %q", c.host, c.tree, c.mount, got, c.want)
		}
	}
}

func TestRemoveStaleMarker(t *testing.T) {
	// marker FILE is removed
	d1 := t.TempDir()
	mf := filepath.Join(d1, ".scion")
	if err := os.WriteFile(mf, []byte("project-id: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleMarker(d1); err != nil {
		t.Fatalf("removeStaleMarker(file): %v", err)
	}
	if _, err := os.Stat(mf); !os.IsNotExist(err) {
		t.Errorf("marker file should be gone, stat err=%v", err)
	}

	// .scion DIRECTORY is left untouched (in-repo git-mode project)
	d2 := t.TempDir()
	md := filepath.Join(d2, ".scion")
	if err := os.Mkdir(md, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleMarker(d2); err != nil {
		t.Fatalf("removeStaleMarker(dir): %v", err)
	}
	if info, err := os.Stat(md); err != nil || !info.IsDir() {
		t.Errorf("marker DIR should be preserved, err=%v", err)
	}

	// absent .scion is a no-op
	if err := removeStaleMarker(t.TempDir()); err != nil {
		t.Errorf("removeStaleMarker(absent): %v", err)
	}
}

func TestRegisterRemovesStaleMarkerBeforeInit(t *testing.T) {
	// A stale marker in the tree must be gone by the time `scion init` runs,
	// so init creates a fresh project (writing workspace_path) rather than
	// resolving the stale marker and skipping it.
	tree := t.TempDir()
	marker := filepath.Join(tree, ".scion")
	if err := os.WriteFile(marker, []byte("project-id: stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(f, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("stale marker should have been removed before init, stat err=%v", err)
	}
}

// TestRegisterRemovesMarkerThroughJailWhenProvided: when Deps.RemoveJailFile is
// set, the register step must remove the stale marker THROUGH it (jail-
// absolute path), NOT rely on the host-side removeStaleMarker fallback. We
// prove "not relied on" by making the fake RemoveJailFile a no-op on the real
// host file: if the code still worked correctly (init ran, no error) while the
// host-side marker file is left physically in place, the host-side remove was
// not part of the path taken.
func TestRegisterRemovesMarkerThroughJailWhenProvided(t *testing.T) {
	tree := t.TempDir()
	marker := filepath.Join(tree, ".scion")
	if err := os.WriteFile(marker, []byte("project-id: stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
	var calls []string
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(f, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
		RemoveJailFile: func(_ context.Context, jailPath string) error {
			calls = append(calls, jailPath)
			return nil // deliberately does NOT touch the host file
		},
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(calls) != 1 || calls[0] != "/lever/.scion" {
		t.Fatalf("RemoveJailFile calls = %+v, want exactly one call with \"/lever/.scion\"", calls)
	}
	// The host-side marker must still be there — proving the host-side
	// removeStaleMarker fallback was NOT exercised alongside RemoveJailFile.
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("host marker should be untouched when RemoveJailFile handles removal, stat err=%v", err)
	}
}

// TestRegisterHostFallbackWhenRemoveJailFileNil pins the pre-existing
// host-side behavior (RemoveJailFile nil, e.g. tests / the broker-only VM
// gate): removeStaleMarker(s.Target) still runs and the marker is gone by the
// time `scion init` runs. This is a regression guard alongside the existing
// TestRegisterRemovesStaleMarkerBeforeInit test.
func TestRegisterHostFallbackWhenRemoveJailFileNil(t *testing.T) {
	tree := t.TempDir()
	marker := filepath.Join(tree, ".scion")
	if err := os.WriteFile(marker, []byte("project-id: stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(f, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
		// RemoveJailFile intentionally left nil.
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("host-side fallback should have removed the marker, stat err=%v", err)
	}
}

// TestRegisterRemovesStaleScionProjectConfigsBeforeInit proves that both
// register-manager and register-grove call Deps.RemoveScionProjectConfigs
// with the target's JAIL workspace path BEFORE `scion init` runs — the
// removal counterpart to the marker-removal race fix above. Without this,
// every apply mints a fresh ~/.scion/project-configs/<uuid> registration and
// the old ones accumulate (the `lever doctor` "duplicate registrations"
// finding).
func TestRegisterRemovesStaleScionProjectConfigsBeforeInit(t *testing.T) {
	tree := t.TempDir()
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
		Groves:  []config.Grove{{Name: "worker", Dir: "groves/worker"}},
	}
	var removeCalls []string
	var initCalls []string
	// Ordering proof: at the moment each RemoveScionProjectConfigs call fires,
	// count how many `scion init --non-interactive` calls the SAME fake runner
	// has already recorded. Since FakeRunner appends to f.Calls synchronously
	// in call order, a count of 0 for the manager's remove call proves it ran
	// before manager init, and a count of 1 for the grove's remove call proves
	// it ran after manager init but before grove init.
	var initCountAtRemove []int
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(f, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
		RemoveScionProjectConfigs: func(_ context.Context, jailWorkspacePath string) error {
			removeCalls = append(removeCalls, jailWorkspacePath)
			n := 0
			for _, c := range f.Calls {
				if strings.Contains(strings.Join(c.Args, " "), "init --non-interactive") {
					n++
				}
			}
			initCountAtRemove = append(initCountAtRemove, n)
			return nil
		},
	}

	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(removeCalls) != 2 {
		t.Fatalf("RemoveScionProjectConfigs calls = %+v, want exactly 2 (manager + grove)", removeCalls)
	}
	if removeCalls[0] != "/lever" {
		t.Errorf("manager remove call path = %q, want /lever", removeCalls[0])
	}
	if removeCalls[1] != "/lever/groves/worker" {
		t.Errorf("grove remove call path = %q, want /lever/groves/worker", removeCalls[1])
	}
	// Manager's remove call must precede ANY init (count 0); the grove's remove
	// call runs after the manager's own init (which already ran, since
	// register-manager completes as one step before register-grove starts) but
	// still before the grove's OWN init (count exactly 1, not 2).
	wantCounts := []int{0, 1}
	for i, n := range initCountAtRemove {
		if n != wantCounts[i] {
			t.Errorf("remove call %d (%s): %d init call(s) had already fired, want %d — it must run before its OWN init", i, removeCalls[i], n, wantCounts[i])
		}
	}

	for _, c := range f.Calls {
		j := strings.Join(c.Args, " ")
		if strings.Contains(j, "init --non-interactive") {
			initCalls = append(initCalls, c.Dir)
		}
	}
	if len(initCalls) != 2 {
		t.Fatalf("init calls = %+v, want exactly 2", initCalls)
	}
}

// TestRegisterToleratesNilRemoveScionProjectConfigs proves the Deps field is
// optional: leaving it nil (as every pre-existing Deps literal in this file
// does) must not crash Run, and `scion init` still runs.
func TestRegisterToleratesNilRemoveScionProjectConfigs(t *testing.T) {
	tree := t.TempDir()
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(f, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
		// RemoveScionProjectConfigs intentionally left nil.
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawInit bool
	for _, c := range f.Calls {
		if strings.Contains(strings.Join(c.Args, " "), "init --non-interactive") {
			sawInit = true
		}
	}
	if !sawInit {
		t.Fatal("scion init should still run when RemoveScionProjectConfigs is nil")
	}
}

func TestRegisterUsesJailPaths(t *testing.T) {
	tree := t.TempDir() // real dir so file-writing steps can write into it
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
		Groves:  []config.Grove{{Name: "worker", Dir: "groves/worker"}},
	}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(f, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var managerInit, groveInit bool
	for _, c := range f.Calls {
		j := strings.Join(c.Args, " ")
		if strings.Contains(j, "init --non-interactive") {
			switch c.Dir {
			case "/lever":
				managerInit = true
			case "/lever/groves/worker":
				groveInit = true
			default:
				t.Errorf("init call used host dir %q, want jail path", c.Dir)
			}
		}
		if strings.Contains(j, "hub link") {
			if c.Dir != "/lever" && c.Dir != "/lever/groves/worker" {
				t.Errorf("hub link call used host dir %q, want jail path", c.Dir)
			}
		}
	}
	if !managerInit {
		t.Errorf("manager init not run with dir /lever")
	}
	if !groveInit {
		t.Errorf("grove init not run with dir /lever/groves/worker")
	}
}

func TestStartUsesJailPath(t *testing.T) {
	tree := t.TempDir() // real dir so file-writing steps can write into it
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: tree,
		Manager: config.Manager{Image: "img"},
	}
	deps := Deps{
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(context.Context, string) error { return nil },
		JailMount: "/lever",
		Scion:     scion.New(f, scion.Options{HubEndpoint: "http://127.0.0.1:8080"}),
	}
	if err := Run(context.Background(), app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawJailG, sawWorkspace bool
	for _, c := range f.Calls {
		j := strings.Join(c.Args, " ")
		if strings.Contains(j, "start hello") {
			if strings.Contains(j, "-g "+tree) {
				t.Errorf("start call used host path: %q", j)
			}
			if strings.Contains(j, "-g /lever") {
				sawJailG = true
			}
			// In-place live mount: the manager must mount the in-jail tree as
			// /workspace, else scion mounts a managed copy of the config dir.
			if strings.Contains(j, "--workspace /lever") {
				sawWorkspace = true
			}
		}
	}
	if !sawJailG {
		t.Fatalf("start call did not use -g /lever; calls=%+v", f.Calls)
	}
	if !sawWorkspace {
		t.Fatalf("start call did not pass --workspace /lever (in-place mount); calls=%+v", f.Calls)
	}
}

func TestDefaultReadCredRejectsWorldReadable(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "tok")
	if err := os.WriteFile(good, []byte("sk-ant-xyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := defaultReadCred(good); err != nil || v != "sk-ant-xyz" {
		t.Fatalf("0600 cred: got %q err %v", v, err)
	}
	bad := filepath.Join(dir, "open")
	if err := os.WriteFile(bad, []byte("sk-ant-xyz"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := defaultReadCred(bad); err == nil {
		t.Fatal("world-readable credential should be rejected")
	}
}
