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
		Name: "hello", Backend: "orbstack", Tree: "/t",
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
		Name: "hello", Backend: "orbstack", Tree: "/t",
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
	_ = os.MkdirAll(filepath.Join(dir, "groves", "worker"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "manager.md"), []byte("Dispatch the worker grove to create HELLO."), 0o644); err != nil {
		t.Fatal(err)
	}
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: dir,
		Manager: config.Manager{Image: "img", PromptFile: "manager.md"},
	}
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

func TestRegisterUsesJailPaths(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: "/tmp/foo",
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
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: "/tmp/foo",
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
			if strings.Contains(j, "-g /tmp/foo") {
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
