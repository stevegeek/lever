package scion

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/exec"
)

func TestEnvSetArgvAndProjectScope(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	c := New(f, Options{})
	if err := c.EnvSet(context.Background(), "/jail/work", "LEVER_LLM_AUTH", "api-key"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(f.Calls))
	}
	got := strings.Join(f.Calls[0].Args, " ")
	// --always is load-bearing: an as_needed var is never projected into the
	// container (scion #944), and no harness declares LEVER_LLM_AUTH, so the
	// env-gather second pass never asks for it either.
	if got != "hub env set --project --always LEVER_LLM_AUTH=api-key" {
		t.Errorf("args = %q", got)
	}
	// Project scope is conveyed by the working directory (bare --project infers it).
	if f.Calls[0].Dir != "/jail/work" {
		t.Errorf("cwd = %q, want /jail/work (project scope)", f.Calls[0].Dir)
	}
}

// TestSecretSetIsAlwaysInjected pins the two properties a Hub secret needs to
// reach the agent intact: an explicit injection mode, and a plaintext value.
// `hub secret set` cannot express the mode at all, so the call goes through the
// --secret form of `hub env set`, which writes the same row.
func TestSecretSetIsAlwaysInjected(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	c := New(f, Options{})
	if err := c.SecretSet(context.Background(), "ANTHROPIC_API_KEY", "sk-ant-placeholder"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	want := "hub env set --secret --always ANTHROPIC_API_KEY sk-ant-placeholder"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
	if strings.Contains(got, "hub secret set") {
		t.Error("hub secret set cannot set an injection mode; the secret would never be projected")
	}
}

// TestSecretSetOldPinErrorNamesTheCause turns scion's 400 into the actual
// problem, which is the pin, not the value.
func TestSecretSetOldPinErrorNamesTheCause(t *testing.T) {
	// scion's real wording, traced through APIError.Error() and the CLI wrapper.
	raw := errors.New("scion hub env set --secret --always K ***: " +
		"Error: failed to set secret: invalid_request: value must be base64-encoded (status: 400)")
	err := secretSetErr("ANTHROPIC_API_KEY", raw)
	if !errors.Is(err, errBase64Pin) {
		t.Fatalf("err = %v, want errBase64Pin", err)
	}
	if !strings.Contains(err.Error(), "ce96122c") {
		t.Errorf("error should name the pin floor: %q", err)
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("error should name the key: %q", err)
	}
	other := errors.New("scion hub env set: connection refused")
	if got := secretSetErr("K", other); got != other {
		t.Errorf("unrelated errors must pass through unchanged, got %v", got)
	}
}

func TestBringupArgv(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	c := New(f, Options{})
	_ = c.InitMachine(context.Background())
	_ = c.ConfigSetGlobal(context.Background(), "image_registry", "scionlocal")
	_ = c.ServerStart(context.Background(), ServerOpts{WebPort: 8080, DevAuth: false})
	_ = c.SecretSet(context.Background(), "CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-rawtoken")
	all := []string{}
	for _, cc := range f.Calls {
		all = append(all, strings.Join(cc.Args, " "))
	}
	j := strings.Join(all, "|")
	for _, want := range []string{
		"init --machine --non-interactive",
		"config set --global image_registry scionlocal",
		"server start --web-port 8080 --dev-auth=false",
		// plaintext: scion stamps encoding=raw since ce96122c, so an encoded
		// value would be stored verbatim.
		"hub env set --secret --always CLAUDE_CODE_OAUTH_TOKEN sk-ant-rawtoken",
	} {
		if !strings.Contains(j, want) {
			t.Fatalf("missing %q in %q", want, j)
		}
	}
}

func TestServerStartArgvWithPort(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	c := New(f, Options{})
	if err := c.ServerStart(context.Background(), ServerOpts{WebPort: 41000, DevAuth: false}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) == 0 {
		t.Fatal("expected at least one call")
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if got != "server start --web-port 41000 --dev-auth=false" {
		t.Errorf("args = %q", got)
	}
}

func TestServerStartArgvWithoutPort(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	c := New(f, Options{})
	if err := c.ServerStart(context.Background(), ServerOpts{DevAuth: true}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) == 0 {
		t.Fatal("expected at least one call")
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if got != "server start --dev-auth=true" {
		t.Errorf("args = %q", got)
	}
}

func TestServerStopArgv(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	c := New(f, Options{})
	if err := c.ServerStop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(f.Calls))
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if got != "server stop" {
		t.Errorf("args = %q", got)
	}
}

// notRunningRunner simulates `scion server stop` failing because no server is
// running (a real non-nil error from the runner, marker text in Stderr — the
// FakeRunner only errors on unscripted commands and can't carry a custom
// error message, so this small wrapper mirrors the alreadyUpRunner pattern in
// internal/apply/run_test.go), falling through to the wrapped FakeRunner for
// everything else.
type notRunningRunner struct {
	*exec.FakeRunner
}

func (r *notRunningRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (exec.Result, error) {
	if name == "scion" && len(args) >= 2 && args[0] == "server" && args[1] == "stop" {
		r.FakeRunner.Calls = append(r.FakeRunner.Calls, exec.Call{Name: name, Args: args, Env: env, Dir: dir})
		return exec.Result{Code: 1, Stderr: "Error: server already exists / not running"}, fmt.Errorf("exit status 1")
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *notRunningRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (exec.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

func TestServerStopTolerantOfNotRunning(t *testing.T) {
	f := &notRunningRunner{FakeRunner: exec.NewFakeRunner()}
	c := New(f, Options{})
	if err := c.ServerStop(context.Background()); err != nil {
		t.Fatalf("ServerStop should tolerate not-running: %v", err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(f.Calls))
	}
}
