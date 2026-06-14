package scion

import (
	"context"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/exec"
)

func TestInitProjectRunsInDir(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion init", exec.Result{})
	c := New(f, Options{})
	if err := c.InitProject(context.Background(), "/g/a"); err != nil {
		t.Fatalf("InitProject: %v", err)
	}
	if f.Calls[0].Dir != "/g/a" || f.Calls[0].Args[0] != "init" {
		t.Fatalf("call=%+v", f.Calls[0])
	}
}

func TestHubLinkArgvAndDir(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion hub link", exec.Result{})
	c := New(f, Options{HubEndpoint: "http://127.0.0.1:8080"})
	if err := c.HubLink(context.Background(), "/g/a"); err != nil {
		t.Fatalf("HubLink: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	for _, want := range []string{"hub link", "--hub http://127.0.0.1:8080", "-y"} {
		if !strings.Contains(got, want) {
			t.Fatalf("argv %q missing %q", got, want)
		}
	}
	if f.Calls[0].Dir != "/g/a" {
		t.Fatalf("dir=%q", f.Calls[0].Dir)
	}
}
