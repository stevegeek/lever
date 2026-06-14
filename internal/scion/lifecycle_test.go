package scion

import (
	"context"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/exec"
)

func TestListParsesAgents(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion list --format json -g /g/a", exec.Result{Stdout: `[{"slug":"a","phase":"running","activity":"building"}]`})
	c := New(f, Options{})
	agents, err := c.List(context.Background(), "/g/a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(agents) != 1 || agents[0].Slug != "a" || agents[0].Phase != "running" || agents[0].Activity != "building" {
		t.Fatalf("agents=%+v", agents)
	}
}

func TestStartArgv(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	c := New(f, Options{})
	err := c.Start(context.Background(), StartOpts{Grove: "a", Task: "do x", Harness: "claude", Project: "/g/a", Image: "img:1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	for _, want := range []string{"-g /g/a", "start a do x", "--harness claude", "--harness-auth oauth-token", "--image img:1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("argv %q missing %q", got, want)
		}
	}
}

func TestResumeStopSuspendArgv(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	c := New(f, Options{})
	_ = c.Resume(context.Background(), "a", "/g/a")
	_ = c.Stop(context.Background(), "a", "/g/a")
	_ = c.Suspend(context.Background(), "a", "/g/a")
	joined := []string{}
	for _, cc := range f.Calls {
		joined = append(joined, strings.Join(cc.Args, " "))
	}
	all := strings.Join(joined, "|")
	for _, want := range []string{"resume a -g /g/a", "stop a -g /g/a", "suspend a -g /g/a"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in %q", want, all)
		}
	}
}

func TestAttachArgvNotRun(t *testing.T) {
	f := exec.NewFakeRunner()
	c := New(f, Options{Bin: "scion"})
	argv := c.AttachArgv("a", "/g/a")
	want := []string{"scion", "attach", "a", "-g", "/g/a"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("argv=%v", argv)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("AttachArgv must NOT execute anything")
	}
}
