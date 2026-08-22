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

// TestServerStartEmitsEnableWebOnly pins the whole web argv: --enable-web
// and nothing else. The absence of --base-url is the point — scion turns
// that flag into the agents' hub endpoint, which no jail agent can reach
// (see ServerOpts.EnableWeb). internal/apply proves the consequence.
func TestServerStartEmitsEnableWebOnly(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	c := New(f, Options{})
	opts := ServerOpts{WebPort: 8080, DevAuth: false, EnableWeb: true}
	if err := c.ServerStart(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) == 0 {
		t.Fatal("expected at least one call")
	}
	got := strings.Join(f.Calls[0].Args, " ")
	want := "server start --web-port 8080 --dev-auth=false --enable-web"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

// A `version:` pin has NO embedded SPA (upstream tracks only
// web/dist/client/.gitkeep), so the hub must be pointed at the assets lever
// built and staged, or it serves its "Web UI Not Available" page.
func TestServerStartEmitsWebAssetsDir(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	c := New(f, Options{})
	opts := ServerOpts{WebPort: 8080, DevAuth: false, EnableWeb: true, WebAssetsDir: "/usr/local/share/scion/web"}
	if err := c.ServerStart(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	want := "server start --web-port 8080 --dev-auth=false --enable-web --web-assets-dir=/usr/local/share/scion/web"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

// --web-assets-dir without --enable-web would be meaningless, and scion treats
// any non-empty value as an override that REPLACES embedded assets rather than
// falling back to them — so the flag never travels alone.
func TestServerStartOmitsWebAssetsDirWithoutEnableWeb(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	c := New(f, Options{})
	opts := ServerOpts{WebPort: 8080, DevAuth: false, WebAssetsDir: "/usr/local/share/scion/web"}
	if err := c.ServerStart(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.Calls[0].Args, " "); strings.Contains(got, "--web-assets-dir") {
		t.Errorf("args = %q, must not carry --web-assets-dir without --enable-web", got)
	}
}

func TestServerStartOmitsWebFlagsByDefault(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("scion", exec.Result{Stdout: "ok"})
	c := New(f, Options{})
	// EnableWeb left at its zero value must not appear in the argv. Note what
	// that does and does not buy: it is NOT how a hub is made API-only —
	// scion's workstation defaults enable the frontend for any start that does
	// not say otherwise, and lever needs them to (see ServerOpts.EnableWeb).
	// What it buys is that --web-assets-dir never travels with it, so the hub
	// falls back to its own embedded assets rather than a directory lever
	// stages only while remote access is on.
	if err := c.ServerStart(context.Background(), ServerOpts{WebPort: 8080, DevAuth: false}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) == 0 {
		t.Fatal("expected at least one call")
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if strings.Contains(got, "--enable-web") || strings.Contains(got, "--base-url") {
		t.Errorf("args = %q, must not contain web flags when EnableWeb is unset", got)
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
// running (a real non-nil error from the runner, scion's own text in Stderr —
// the FakeRunner only errors on unscripted commands and can't carry a custom
// error message, so this small wrapper mirrors the alreadyUpRunner pattern in
// internal/apply/run_test.go), falling through to the wrapped FakeRunner for
// everything else.
type notRunningRunner struct {
	*exec.FakeRunner
	stderr string
}

func (r *notRunningRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (exec.Result, error) {
	if name == "scion" && len(args) >= 2 && args[0] == "server" && args[1] == "stop" {
		r.FakeRunner.Calls = append(r.FakeRunner.Calls, exec.Call{Name: name, Args: args, Env: env, Dir: dir})
		return exec.Result{Code: 1, Stderr: r.stderr}, fmt.Errorf("exit status 1")
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *notRunningRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (exec.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// TestServerStopTolerantOfNotRunning uses scion's REAL wording, which is the
// whole point of the test.
//
// It previously used a hand-written "Error: server already exists / not
// running", which passed on the "already exists" substring — so it asserted
// the tolerance while proving nothing about what scion actually says. A live
// apply found the gap: `scion server stop` on a stopped daemon says "server
// daemon is not running" (cmd/server_daemon.go), matching neither arm of
// AlreadyRunning, and the error failed the whole apply.
func TestServerStopTolerantOfNotRunning(t *testing.T) {
	// Both shapes scion emits: the bare stop message, and the restart variant
	// that appends a hint.
	for _, stderr := range []string{
		"Error: server daemon is not running",
		"Error: server daemon is not running\n\nUse 'scion server start' to start it",
	} {
		f := &notRunningRunner{FakeRunner: exec.NewFakeRunner(), stderr: stderr}
		c := New(f, Options{})
		if err := c.ServerStop(context.Background()); err != nil {
			t.Fatalf("ServerStop must tolerate scion's own not-running answer %q: %v", stderr, err)
		}
		if len(f.Calls) != 1 {
			t.Fatalf("want 1 call, got %d", len(f.Calls))
		}
	}
}

// A stop that fails for any OTHER reason is still a failure: tolerance is for
// "there was nothing to stop", not for "the stop did not work".
func TestServerStopReportsRealFailures(t *testing.T) {
	f := &notRunningRunner{FakeRunner: exec.NewFakeRunner(), stderr: "Error: permission denied"}
	c := New(f, Options{})
	if err := c.ServerStop(context.Background()); err == nil {
		t.Fatal("ServerStop swallowed a real failure")
	}
}

// AlreadyRunning must NOT learn the not-running wording: it also guards
// ServerStart, where a daemon reported as not running means the start failed.
func TestAlreadyRunningDoesNotCoverNotRunning(t *testing.T) {
	err := fmt.Errorf("scion server start: Error: server daemon is not running")
	if AlreadyRunning(err) {
		t.Fatal("AlreadyRunning matched a not-running error — a failed start would be swallowed as success")
	}
	if !notRunning(err) {
		t.Fatal("notRunning did not match scion's wording")
	}
	if notRunning(fmt.Errorf("agent 'x' is not running (phase: stopped)")) {
		t.Fatal("notRunning matched an AGENT-level message; it must only cover the daemon")
	}
}
