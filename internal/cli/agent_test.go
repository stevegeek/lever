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
