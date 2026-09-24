package scion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion/layout"
)

// okScion returns a runner that answers every scion verb with success, and a
// client over it, for tests that only inspect the argv.
func okScion() (*proc.FakeRunner, *Client) {
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: "ok"})
	return f, New(f, Options{})
}

func TestEnvSetArgvAndProjectScope(t *testing.T) {
	f, c := okScion()
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
	f, c := okScion()
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
	f, c := okScion()
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
	f, c := okScion()
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
	f, c := okScion()
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
	f, c := okScion()
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
	f, c := okScion()
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
	f, c := okScion()
	opts := ServerOpts{WebPort: 8080, DevAuth: false, WebAssetsDir: "/usr/local/share/scion/web"}
	if err := c.ServerStart(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.Calls[0].Args, " "); strings.Contains(got, "--web-assets-dir") {
		t.Errorf("args = %q, must not carry --web-assets-dir without --enable-web", got)
	}
}

// The session-cookie signing key travels in the argv, equals form, so scion's
// daemon persists it to server-args.json and a `scion server restart` replays
// it — the whole point: sessions survive hub restarts.
func TestServerStartEmitsSessionSecret(t *testing.T) {
	f, c := okScion()
	opts := ServerOpts{WebPort: 8080, DevAuth: false, SessionSecret: "sessionsecrethex"}
	if err := c.ServerStart(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	want := "server start --web-port 8080 --dev-auth=false --session-secret=sessionsecrethex"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

// An empty SessionSecret omits the flag entirely (scion generates a per-boot
// random key) — the throwaway mint-window hub takes this path.
func TestServerStartOmitsSessionSecretWhenEmpty(t *testing.T) {
	f, c := okScion()
	if err := c.ServerStart(context.Background(), ServerOpts{WebPort: 8080, DevAuth: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.Calls[0].Args, " "); strings.Contains(got, "--session-secret") {
		t.Errorf("args = %q, must not carry --session-secret when unset", got)
	}
}

// A failed start's error renders the argv, and the argv carries the secret —
// ServerStart must scrub it (runSecret), because redactArgs only knows the
// hub-secret-set argv shapes.
func TestServerStartFailureRedactsSessionSecret(t *testing.T) {
	// Nothing scripted: the fake fails every call, which drives run's error
	// path — the one that renders the argv.
	f := proc.NewFakeRunner()
	c := New(f, Options{})
	err := c.ServerStart(context.Background(), ServerOpts{WebPort: 8080, DevAuth: false, SessionSecret: "sessionsecrethex"})
	if err == nil {
		t.Fatal("want error from failed server start")
	}
	if strings.Contains(err.Error(), "sessionsecrethex") {
		t.Fatal("server-start error leaks the session secret")
	}
}

func TestServerStartOmitsWebFlagsByDefault(t *testing.T) {
	f, c := okScion()
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

// TestServerStartNeverDisablesTheWebFrontend guards a dependency that lever
// rested on for the whole remote-access arc without writing it down, and that
// a plausible tidy-up would break: the flag must be OMITTED when lever does
// not want the SPA, never sent as --enable-web=false.
//
// scion's workstation defaults enable the web frontend for any non-hosted
// start that does not say otherwise, and lever needs exactly that. With the
// frontend off, the Hub API is no longer mounted on the web server and binds
// cfg.Hub.Port — 9810 — instead (cmd/server_foreground.go, the !enableWeb
// branch), while the broker, every agent's SCION_HUB_ENDPOINT, `lever doctor`
// and the remote proxy all dial 8080. So "converging" the flag the way
// DevAuth is converged — explicitly, both ways, which is right for DevAuth —
// would take the instance down at the next apply.
//
// A test rather than only a comment because the comment on ServerOpts.EnableWeb
// asserted the opposite of the truth for the length of the branch, and prose is
// one refactor away from being lost (docs/2026-08-18-comment-drift-remote-access.md).
func TestServerStartNeverDisablesTheWebFrontend(t *testing.T) {
	for _, o := range []ServerOpts{
		{},
		{WebPort: 8080, DevAuth: false},
		{WebPort: 8080, DevAuth: true},
		{WebPort: 8080, EnableWeb: true},
		{WebPort: 8080, EnableWeb: true, WebAssetsDir: "/usr/local/share/scion/web"},
		{WebPort: 8080, WebAssetsDir: "/usr/local/share/scion/web"},
	} {
		f, c := okScion()
		if err := c.ServerStart(context.Background(), o); err != nil {
			t.Fatalf("%+v: %v", o, err)
		}
		if len(f.Calls) == 0 {
			t.Fatalf("%+v: expected at least one call", o)
		}
		got := strings.Join(f.Calls[0].Args, " ")
		if strings.Contains(got, "--enable-web=false") {
			t.Fatalf("args = %q: this moves the Hub API off the web port and takes the instance down", got)
		}
	}
}

func TestServerStopArgv(t *testing.T) {
	f, c := okScion()
	f.Script("sh -c f=", proc.Result{}) // no live pid: nothing to wait on
	if err := c.ServerStop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 2 {
		t.Fatalf("want 2 calls (pid probe, stop), got %d: %+v", len(f.Calls), f.Calls)
	}
	if probe := f.Calls[0]; probe.Name != "sh" || probe.Args[len(probe.Args)-1] != layout.ServerPIDRel {
		t.Errorf("first call = %+v, want the pid probe on %s", probe, layout.ServerPIDRel)
	}
	got := strings.Join(f.Calls[1].Args, " ")
	if got != "server stop" {
		t.Errorf("args = %q", got)
	}
}

// A live scion pid is waited on after the stop, so the next start does not
// race the old daemon for its ports (scion's stop returns after 500 ms).
func TestServerStopWaitsForTheOldPid(t *testing.T) {
	f, c := okScion()
	f.Script("sh -c f=", proc.Result{Stdout: "4242"})
	f.Script("sh -c i=0", proc.Result{})
	if err := c.ServerStop(context.Background()); err != nil {
		t.Fatal(err)
	}
	iStop := f.CallIndex(proc.ArgvPrefix("scion", "server", "stop"))
	iWait := f.CallIndex(proc.ArgvContains("i=0", "_ 4242"))
	if iStop < 0 || iWait < 0 || iWait < iStop {
		t.Fatalf("want stop then a wait on pid 4242; calls=%+v", f.Calls)
	}
}

// A daemon that outlives the bounded wait fails the stop: the caller's next
// start would hit a port conflict.
func TestServerStopFailsWhenTheOldPidLingers(t *testing.T) {
	f, c := okScion()
	f.Script("sh -c f=", proc.Result{Stdout: "4242"})
	// "sh -c i=0" unscripted: the fake fails it, as the script's exit 1 would.
	err := c.ServerStop(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pid 4242") {
		t.Fatalf("err = %v, want a failure naming pid 4242", err)
	}
}

// A pid file whose pid is not a scion server is removed by the probe and is
// neither signalled by lever nor waited on.
func TestServerStopSkipsAStalePid(t *testing.T) {
	f, c := okScion()
	f.Script("sh -c f=", proc.Result{Stdout: "stale:77"})
	if err := c.ServerStop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Called(proc.ArgvContains("i=0")) {
		t.Fatalf("waited on a stale pid; calls=%+v", f.Calls)
	}
}

// The probe script itself, against a real shell and /proc (Linux only): a
// pid file naming a live process that is not a scion server is removed.
func TestServerPIDProbeScriptRemovesAStalePidFile(t *testing.T) {
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("needs /proc")
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, layout.Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()
	pidFile := filepath.Join(home, layout.ServerPIDRel)
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(sleeper.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", serverPIDProbeScript, "_", layout.ServerPIDRel)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.HasPrefix(string(out), "stale:") {
		t.Fatalf("probe printed %q, want stale:<pid>", out)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("stale pid file still there (stat err %v)", err)
	}
}

// The throwaway dev-auth hub's argv: web off, so the Hub API binds --port and
// every non-public route needs the Bearer dev token.
func TestServerStartArgvWebDisabled(t *testing.T) {
	f, c := okScion()
	if err := c.ServerStart(context.Background(), ServerOpts{HubPort: 48080, DisableWeb: true, DevAuth: true, Exclusive: true}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if got != "server start --port 48080 --enable-web=false --dev-auth=true" {
		t.Errorf("args = %q", got)
	}
}

func TestServerStartRejectsEnableAndDisableWeb(t *testing.T) {
	f, c := okScion()
	if err := c.ServerStart(context.Background(), ServerOpts{EnableWeb: true, DisableWeb: true}); err == nil {
		t.Fatal("want an error for EnableWeb with DisableWeb")
	}
	if len(f.Calls) != 0 {
		t.Fatalf("ran %d call(s) for an invalid option set", len(f.Calls))
	}
}

// notRunningRunner simulates `scion server stop` failing because no server is
// running (a real non-nil error from the runner, scion's own text in Stderr —
// the FakeRunner only errors on unscripted commands and can't carry a custom
// error message, so this small wrapper mirrors the alreadyUpRunner pattern in
// internal/apply/run_test.go), falling through to the wrapped FakeRunner for
// everything else.
type notRunningRunner struct {
	*proc.FakeRunner
	stderr string
}

func (r *notRunningRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "scion" && len(args) >= 2 && args[0] == "server" && args[1] == "stop" {
		r.FakeRunner.Calls = append(r.FakeRunner.Calls, proc.Call{Name: name, Args: args, Env: env, Dir: dir})
		return proc.Result{Code: 1, Stderr: r.stderr}, fmt.Errorf("exit status 1")
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *notRunningRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
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
		f := &notRunningRunner{FakeRunner: proc.NewFakeRunner(), stderr: stderr}
		c := New(f, Options{})
		if err := c.ServerStop(context.Background()); err != nil {
			t.Fatalf("ServerStop must tolerate scion's own not-running answer %q: %v", stderr, err)
		}
		if n := countScion(f.Calls); n != 1 {
			t.Fatalf("want 1 scion call, got %d", n)
		}
	}
}

func countScion(calls []proc.Call) int {
	n := 0
	for _, c := range calls {
		if c.Name == "scion" {
			n++
		}
	}
	return n
}

// A stop that fails for any OTHER reason is still a failure: tolerance is for
// "there was nothing to stop", not for "the stop did not work".
func TestServerStopReportsRealFailures(t *testing.T) {
	f := &notRunningRunner{FakeRunner: proc.NewFakeRunner(), stderr: "Error: permission denied"}
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

// assertPredicate checks an error-classifying predicate against the wordings
// it must accept and the errors it must reject.
func assertPredicate(t *testing.T, name string, pred func(error) bool, accept []string, reject []error) {
	t.Helper()
	for _, msg := range accept {
		if !pred(errors.New(msg)) {
			t.Errorf("%s: %q must match", name, msg)
		}
	}
	for _, err := range reject {
		if pred(err) {
			t.Errorf("%s: %v must not match", name, err)
		}
	}
}

func TestIsBrokerUnavailable(t *testing.T) {
	assertPredicate(t, "IsBrokerUnavailable", IsBrokerUnavailable,
		[]string{
			"no_runtime_broker",
			"start-manager: No runtime brokers available",
			"resume: no runtime broker available",
			"context deadline exceeded from the Hub during start-manager",
		},
		[]error{nil, errors.New("agent 'x' is not running"), errors.New("permission denied")})
}

func TestIsAgentAbsent(t *testing.T) {
	assertPredicate(t, "IsAgentAbsent", IsAgentAbsent,
		[]string{
			"Hub is not responding",
			"dial tcp 127.0.0.1:8080: connect: Connection Refused",
			"hub: Project Not Found (404)",
			"no git origin remote found",
		},
		[]error{nil, errors.New("No runtime brokers available"), errors.New("timeout")})
}

// alreadyRunningRunner answers every `scion server start` the way scion does
// when the jail's one server pid file names a live daemon.
type alreadyRunningRunner struct{ *proc.FakeRunner }

func (r *alreadyRunningRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "scion" && len(args) >= 2 && args[0] == "server" && args[1] == "start" {
		r.FakeRunner.Calls = append(r.FakeRunner.Calls, proc.Call{Name: name, Args: args, Env: env, Dir: dir})
		return proc.Result{Code: 1, Stderr: "Error: server is already running (PID: 1731)"}, fmt.Errorf("exit status 1")
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

// An exclusive start (the bootstrap throwaway) must fail on "already
// running": that answer is about ANOTHER hub, and tolerating it made apply
// wait on a port nothing binds and then stop the live hub on cleanup.
func TestServerStartExclusiveRefusesAlreadyRunning(t *testing.T) {
	r := &alreadyRunningRunner{FakeRunner: proc.NewFakeRunner()}
	c := New(r, Options{})
	err := c.ServerStart(context.Background(), ServerOpts{WebPort: 48080, DevAuth: true, Exclusive: true})
	if err == nil || !AlreadyRunning(err) {
		t.Fatalf("exclusive ServerStart err = %v, want an already-running error", err)
	}
}
