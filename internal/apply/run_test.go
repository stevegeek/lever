package apply

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/config"
	"github.com/lever-to/lever/internal/exec"
	"github.com/lever-to/lever/internal/scion"
)

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
	if err := os.WriteFile(cfg, []byte("name: hello\nbackend: orbstack\ntree: workspace\nmanager:\n  image: img\n  prompt_file: manager.md\n"), 0o644); err != nil {
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
	var sawEnvSet bool
	var startArgv string
	for _, c := range f.Calls {
		argv := strings.Join(c.Args, " ")
		if argv == "hub env set --project LEVER_LLM_AUTH=api-key" {
			sawEnvSet = true
		}
		if c.Name == "scion" && strings.Contains(argv, " start ") {
			startArgv = argv
		}
	}
	if !sawEnvSet {
		t.Fatalf("api-key manager: expected LEVER_LLM_AUTH env set; calls=%+v", f.Calls)
	}
	// api-key manager must start with --no-auth (no oauth-token gather).
	if !strings.Contains(startArgv, "--no-auth") || strings.Contains(startArgv, "oauth-token") {
		t.Fatalf("api-key manager start must use --no-auth (not oauth-token); argv=%q", startArgv)
	}
}

func TestStartManagerNoLLMAuthEnvForSubscription(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "workspace"), 0o755)
	cfg := filepath.Join(dir, config.CanonicalName)
	if err := os.WriteFile(cfg, []byte("name: hello\nbackend: orbstack\ntree: workspace\nmanager:\n  image: img\n"), 0o644); err != nil {
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

func TestRegisterUsesJailPaths(t *testing.T) {
	tree := t.TempDir() // real dir so the write-manifest step can write into it
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
	tree := t.TempDir() // real dir so the write-manifest step can write into it
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
