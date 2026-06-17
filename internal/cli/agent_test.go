package cli

import (
	"bytes"
	"strings"
	"testing"

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
