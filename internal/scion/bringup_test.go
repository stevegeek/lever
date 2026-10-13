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

// serverStartCall is the `scion server start` call (ServerStart probes the
// pid file before it).
func serverStartCall(t *testing.T, f *proc.FakeRunner) proc.Call {
	t.Helper()
	i := f.CallIndex(proc.ArgvPrefix("scion", "server", "start"))
	if i < 0 {
		t.Fatalf("no server start call; calls=%+v", f.Calls)
	}
	return f.Calls[i]
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
	// --always is required: an as_needed var no harness declares is never
	// projected into the container.
	if got != "hub env set --project --always LEVER_LLM_AUTH=api-key" {
		t.Errorf("args = %q", got)
	}
	// Bare --project infers the project from the working directory.
	if f.Calls[0].Dir != "/jail/work" {
		t.Errorf("cwd = %q, want /jail/work (project scope)", f.Calls[0].Dir)
	}
}

// TestSecretSetIsAlwaysInjected pins the two properties a Hub secret needs to
// reach the agent intact: an explicit injection mode, and a plaintext value.
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
	got := strings.Join(serverStartCall(t, f).Args, " ")
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
	got := strings.Join(serverStartCall(t, f).Args, " ")
	if got != "server start --dev-auth=true" {
		t.Errorf("args = %q", got)
	}
}

// TestServerStartEmitsEnableWebOnly pins the whole web argv: --enable-web
// and nothing else, in particular no --base-url (see ServerOpts.EnableWeb).
func TestServerStartEmitsEnableWebOnly(t *testing.T) {
	f, c := okScion()
	opts := ServerOpts{WebPort: 8080, DevAuth: false, EnableWeb: true}
	if err := c.ServerStart(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) == 0 {
		t.Fatal("expected at least one call")
	}
	got := strings.Join(serverStartCall(t, f).Args, " ")
	want := "server start --web-port 8080 --dev-auth=false --enable-web"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

// A version: pin has no embedded SPA, so the hub must be pointed at the
// assets lever staged.
func TestServerStartEmitsWebAssetsDir(t *testing.T) {
	f, c := okScion()
	opts := ServerOpts{WebPort: 8080, DevAuth: false, EnableWeb: true, WebAssetsDir: "/usr/local/share/scion/web"}
	if err := c.ServerStart(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(serverStartCall(t, f).Args, " ")
	want := "server start --web-port 8080 --dev-auth=false --enable-web --web-assets-dir=/usr/local/share/scion/web"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

// --web-assets-dir never travels without --enable-web: scion treats any
// non-empty value as replacing the embedded assets.
func TestServerStartOmitsWebAssetsDirWithoutEnableWeb(t *testing.T) {
	f, c := okScion()
	opts := ServerOpts{WebPort: 8080, DevAuth: false, WebAssetsDir: "/usr/local/share/scion/web"}
	if err := c.ServerStart(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(serverStartCall(t, f).Args, " "); strings.Contains(got, "--web-assets-dir") {
		t.Errorf("args = %q, must not carry --web-assets-dir without --enable-web", got)
	}
}

// The session-cookie signing key travels in the argv so scion's daemon replays
// it on restart and sessions survive.
func TestServerStartEmitsSessionSecret(t *testing.T) {
	f, c := okScion()
	opts := ServerOpts{WebPort: 8080, DevAuth: false, SessionSecret: "sessionsecrethex"}
	if err := c.ServerStart(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(serverStartCall(t, f).Args, " ")
	want := "server start --web-port 8080 --dev-auth=false --session-secret=sessionsecrethex"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestServerStartOmitsSessionSecretWhenEmpty(t *testing.T) {
	f, c := okScion()
	if err := c.ServerStart(context.Background(), ServerOpts{WebPort: 8080, DevAuth: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(serverStartCall(t, f).Args, " "); strings.Contains(got, "--session-secret") {
		t.Errorf("args = %q, must not carry --session-secret when unset", got)
	}
}

// A failed start's error renders the argv, which carries the secret:
// ServerStart must scrub it (runSecret).
func TestServerStartFailureRedactsSessionSecret(t *testing.T) {
	// Nothing scripted: the fake fails every call.
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
	// EnableWeb at its zero value must not appear in the argv. This does not
	// make the hub API-only (see ServerOpts.EnableWeb); it keeps
	// --web-assets-dir off.
	if err := c.ServerStart(context.Background(), ServerOpts{WebPort: 8080, DevAuth: false}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) == 0 {
		t.Fatal("expected at least one call")
	}
	got := strings.Join(serverStartCall(t, f).Args, " ")
	if strings.Contains(got, "--enable-web") || strings.Contains(got, "--base-url") {
		t.Errorf("args = %q, must not contain web flags when EnableWeb is unset", got)
	}
}

// TestServerStartNeverDisablesTheWebFrontend: the live hub must omit
// --enable-web, never send --enable-web=false. With the frontend off the Hub
// API moves off port 8080, which the broker, agents, doctor and the remote
// proxy all dial, so converging the flag both ways like DevAuth would take
// the instance down.
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
		got := strings.Join(serverStartCall(t, f).Args, " ")
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
	got := strings.Join(serverStartCall(t, f).Args, " ")
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

// notRunningRunner simulates scion server stop failing because no server is
// running; FakeRunner cannot carry a custom error message.
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

// TestServerStopTolerantOfNotRunning must use scion's real wording
// (cmd/server_daemon.go); a hand-written message proves nothing.
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

// An exclusive start must fail on "already running": that answer is about
// another hub, and tolerating it would stop the live hub on cleanup.
func TestServerStartExclusiveRefusesAlreadyRunning(t *testing.T) {
	r := &alreadyRunningRunner{FakeRunner: proc.NewFakeRunner()}
	c := New(r, Options{})
	err := c.ServerStart(context.Background(), ServerOpts{WebPort: 48080, DevAuth: true, Exclusive: true})
	if err == nil || !AlreadyRunning(err) {
		t.Fatalf("exclusive ServerStart err = %v, want an already-running error", err)
	}
}

func TestIsRefusedByHub(t *testing.T) {
	for _, s := range []string{
		`failed to start agent via Hub: conflict: agent "m" already exists in this project (status: 409)`,
		`failed to suspend agent via Hub: forbidden: Insufficient permissions (status: 403)`,
		`Missing required scope: agent:lifecycle`,
	} {
		if !IsRefusedByHub(errors.New(s)) {
			t.Errorf("IsRefusedByHub(%q) = false", s)
		}
	}
	for _, s := range []string{
		`conflict: Agent name is already in use by a stopped container. Please delete the existing agent or choose a different name. (status: 409)`,
		`Failed to resume agent: container state corrupt`,
		`authentication failed, login to hub`,
		`cannot resume agent 'm': agent does not exist`,
	} {
		if IsRefusedByHub(errors.New(s)) {
			t.Errorf("IsRefusedByHub(%q) = true; it must still take the recovery path", s)
		}
	}
	if IsRefusedByHub(nil) {
		t.Error("IsRefusedByHub(nil) = true")
	}
}

// TestErrSummary: the cause line survives; the settings warnings and the
// usage dump do not; the result is one bounded line.
func TestErrSummary(t *testing.T) {
	raw := "scion resume m -g /lever: Resuming agent 'm'...\n" +
		"2026/10/02 15:25:35 WARN settings file contains unrecognized keys (these will be ignored) keys=\"[a b]\"\n" +
		"Using hub: http://127.0.0.1:8080\n" +
		"Error: failed to start agent via Hub: runtime_error: Failed to resume suspended agent\r\n" +
		"Usage:\n  scion resume <agent> [flags]\n      --force   Resume an agent in the error phase\n"
	got := ErrSummary(errors.New(raw))
	for _, want := range []string{"scion resume m", "Failed to resume suspended agent"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q lacks %q", got, want)
		}
	}
	for _, not := range []string{"WARN", "Usage", "--force", "\n"} {
		if strings.Contains(got, not) {
			t.Errorf("summary %q still has %q", got, not)
		}
	}
	if long := ErrSummary(errors.New(strings.Repeat("x", 5000))); len(long) > 610 {
		t.Errorf("summary of a 5000-byte error is %d bytes", len(long))
	}
	if ErrSummary(nil) != "" {
		t.Error("nil error has a summary")
	}
}
