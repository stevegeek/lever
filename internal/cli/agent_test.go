package cli

import (
	"bytes"
	"strings"
	"testing"

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

func TestAgentStartResolvesImageFromConfig(t *testing.T) {
	orig := loadAppConfig
	loadAppConfig = func(path string) (*config.App, error) {
		return &config.App{
			Manager: config.Manager{Image: "scionlocal/lever-claude:latest"},
			Groves: []config.Grove{
				{Name: "scratch", Dir: "groves/scratch"},
				{Name: "rust", Dir: "groves/rust", Image: "scionlocal/lever-rust:latest"},
			},
		}, nil
	}
	defer func() { loadAppConfig = orig }()

	cases := []struct {
		name      string
		grove     string
		wantImage string
	}{
		{"inherits manager image", "scratch", "scionlocal/lever-claude:latest"},
		{"uses grove override", "rust", "scionlocal/lever-rust:latest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := exec.NewFakeRunner()
			f.Script("scion", exec.Result{})
			root := newManagerRootWith(clientWith(f))
			root.SetArgs([]string{"agent", "start", tc.grove, "--config", "/x/lever.yaml", "-g", "groves/" + tc.grove})
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

func TestAgentStartExplicitImageWinsOverConfig(t *testing.T) {
	orig := loadAppConfig
	loadAppConfig = func(path string) (*config.App, error) {
		return &config.App{Manager: config.Manager{Image: "from-config:1"},
			Groves: []config.Grove{{Name: "scratch", Dir: "groves/scratch"}}}, nil
	}
	defer func() { loadAppConfig = orig }()

	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	root := newManagerRootWith(clientWith(f))
	root.SetArgs([]string{"agent", "start", "scratch", "--config", "/x/lever.yaml", "--image", "explicit:9"})
	if err := root.Execute(); err != nil {
		t.Fatalf("start: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if !strings.Contains(got, "--image explicit:9") || strings.Contains(got, "from-config:1") {
		t.Fatalf("argv=%q want explicit image to win", got)
	}
}

func TestAgentStartNoConfigOmitsImage(t *testing.T) {
	// no --config, no $LEVER_CONFIG in test env, no --image — flag default
	// reads the env at construction, so clear it BEFORE building the root.
	t.Setenv("LEVER_CONFIG", "")
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	root := newManagerRootWith(clientWith(f))
	root.SetArgs([]string{"agent", "start", "scratch", "-g", "groves/scratch"})
	if err := root.Execute(); err != nil {
		t.Fatalf("start: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if strings.Contains(got, "--image") {
		t.Fatalf("argv=%q should not contain --image without config", got)
	}
}

func TestAgentStartDiscoversConfigForImage(t *testing.T) {
	t.Setenv("LEVER_CONFIG", "") // force discovery path, not env
	dir := instanceDir(t, "demo")
	t.Chdir(dir)

	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	root := newManagerRootWith(clientWith(f))
	// no --config and no --image: image must be resolved from the discovered config
	root.SetArgs([]string{"agent", "start", "anygrove", "-g", "groves/anygrove"})
	if err := root.Execute(); err != nil {
		t.Fatalf("start: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if !strings.Contains(got, "--image img:1") {
		t.Fatalf("argv=%q want --image img:1 (manager image inherited via discovery)", got)
	}
}
