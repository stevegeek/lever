package apply

import (
	"context"
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
	if want := "hub secret set CLAUDE_CODE_OAUTH_TOKEN sk-ant-raw"; !strings.Contains(j, want) {
		t.Fatalf("missing scion call %q in: %q", want, j)
	}
}
