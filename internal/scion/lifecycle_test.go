package scion

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/exec"
)

func TestContainerLive(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"running", true}, {"Up 6 seconds", true}, {"Up About a minute", true},
		{"stopped", false}, {"Exited (1) 4 minutes ago", false}, {"", false},
	} {
		if got := ContainerLive(c.in); got != c.want {
			t.Fatalf("ContainerLive(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestListParsesAgents(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion list --format json -g /g/a --non-interactive", exec.Result{Stdout: `[{"slug":"a","phase":"running","activity":"building"}]`})
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
	err := c.Start(context.Background(), StartOpts{Worker: "a", Task: "do x", Harness: "claude", Project: "/g/a", Image: "img:1", Workspace: "/lever"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	for _, want := range []string{"-g /g/a", "start a do x", "--harness claude", "--harness-auth oauth-token", "--image img:1", "--workspace /lever"} {
		if !strings.Contains(got, want) {
			t.Fatalf("argv %q missing %q", got, want)
		}
	}
}

func TestStartWorkspaceSubdirEmitsRelativeFlag(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	c := New(f, Options{})
	if err := c.Start(context.Background(), StartOpts{Worker: "a", Task: "x", Project: "/g/a", WorkspaceSubdir: "workers/a"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if !strings.Contains(got, "--workspace-subdir workers/a") {
		t.Fatalf("argv %q must contain --workspace-subdir workers/a", got)
	}
	if strings.Contains(got, "--workspace /") {
		t.Fatalf("argv %q must not emit an absolute --workspace when a subdir is set (scion ignores the subdir if both are given)", got)
	}
}

func TestStartWorkspaceSubdirWinsOverWorkspace(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	c := New(f, Options{})
	if err := c.Start(context.Background(), StartOpts{Worker: "a", Task: "x", Project: "/g/a", Workspace: "/lever", WorkspaceSubdir: "workers/a"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if !strings.Contains(got, "--workspace-subdir workers/a") || strings.Contains(got, "--workspace /lever") {
		t.Fatalf("subdir must take precedence over absolute workspace; argv %q", got)
	}
}

func TestStartAPIKeyUsesAPIKeyAuth(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	c := New(f, Options{})
	// api-key mode: scion starts with --harness-auth api-key, satisfied by a
	// placeholder ANTHROPIC_API_KEY (Hub secret); the real credential is the
	// in-container broker capability token (settings.json). Must NOT request
	// oauth-token (no CLAUDE_CODE_OAUTH_TOKEN exists in api-key mode).
	if err := c.Start(context.Background(), StartOpts{Worker: "a", Task: "x", Harness: "claude", Project: "/g/a", APIKey: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if !strings.Contains(got, "--harness-auth api-key") {
		t.Fatalf("api-key Start argv %q must use --harness-auth api-key", got)
	}
	if strings.Contains(got, "oauth-token") {
		t.Fatalf("api-key Start argv %q must NOT request oauth-token auth", got)
	}
}

func TestStartOmitsWorkspaceWhenEmpty(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	c := New(f, Options{})
	if err := c.Start(context.Background(), StartOpts{Worker: "a", Task: "x", Project: "/g/a"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := strings.Join(f.Calls[0].Args, " "); strings.Contains(got, "--workspace") {
		t.Fatalf("argv %q should not contain --workspace when Workspace empty", got)
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

func TestListParsesContainerStatusAndIgnoresUnknownFields(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion list --format json -g /lever --non-interactive", exec.Result{Stdout: `[
		{"slug":"assistant","phase":"running","containerStatus":"running","other":"ignored"},
		{"slug":"scratch","phase":"suspended","containerStatus":"stopped"}
	]`})
	c := New(f, Options{})
	agents, err := c.List(context.Background(), "/lever")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if want := "list --format json -g /lever --non-interactive"; got != want {
		t.Fatalf("argv = %q, want exactly %q", got, want)
	}
	if len(agents) != 2 {
		t.Fatalf("agents=%+v", agents)
	}
	if agents[0].Slug != "assistant" || agents[0].Phase != "running" || agents[0].ContainerStatus != "running" {
		t.Fatalf("agents[0]=%+v", agents[0])
	}
	if agents[1].Slug != "scratch" || agents[1].Phase != "suspended" || agents[1].ContainerStatus != "stopped" {
		t.Fatalf("agents[1]=%+v", agents[1])
	}
}

func TestListEmptyStdoutIsEmptySlice(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion list --format json -g /lever --non-interactive", exec.Result{Stdout: "   \n"})
	c := New(f, Options{})
	agents, err := c.List(context.Background(), "/lever")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("agents=%+v, want empty slice", agents)
	}
}

func TestListMalformedJSONErrors(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion list --format json -g /lever --non-interactive", exec.Result{Stdout: `[{"slug": "a", `})
	c := New(f, Options{})
	if _, err := c.List(context.Background(), "/lever"); err == nil {
		t.Fatal("expected error parsing malformed JSON")
	}
}

func TestDeleteArgv(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{})
	c := New(f, Options{})
	if err := c.Delete(context.Background(), "scratch", "/lever"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if want := "delete scratch -g /lever --non-interactive"; got != want {
		t.Fatalf("argv = %q, want exactly %q", got, want)
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

// TestAttachArgvEmbedsHubTokenWhenPresent: the attach/TTY path bypasses
// Client.env() (it's exec()'d directly, never through the runner), so the
// controller PAT must be embedded into the returned argv itself — mirroring
// how the jail env is embedded for attach (internal/jail/attach.go).
func TestAttachArgvEmbedsHubTokenWhenPresent(t *testing.T) {
	f := exec.NewFakeRunner()
	c := New(f, Options{Bin: "scion", HubToken: "pat123"})
	argv := c.AttachArgv("a", "/g/a")
	want := []string{"env", "SCION_HUB_TOKEN=pat123", "scion", "attach", "a", "-g", "/g/a"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("argv=%v, want=%v", argv, want)
	}
}

// TestAttachArgvOmitsHubTokenWhenEmpty: no token means no env prefix at all —
// exact argv shape preserved (see TestAttachArgvNotRun) so subscription-mode
// attach is unaffected.
func TestAttachArgvOmitsHubTokenWhenEmpty(t *testing.T) {
	f := exec.NewFakeRunner()
	c := New(f, Options{Bin: "scion"})
	argv := c.AttachArgv("a", "/g/a")
	for _, tok := range argv {
		if strings.HasPrefix(tok, "SCION_HUB_TOKEN=") || tok == "env" {
			t.Fatalf("argv=%v should not contain an env/SCION_HUB_TOKEN prefix when no token set", argv)
		}
	}
}
