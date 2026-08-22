package cli

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	leverexec "github.com/stevegeek/lever/internal/exec"
	"github.com/stevegeek/lever/internal/scion"
)

// writeTmpConfig writes a minimal app.yaml with a real tree directory structure
// and returns the config file path. Mirrors config_test.go's writeTmp.
func writeTmpConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "workers", "worker"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `name: demo
backend: orbstack
tree: ./tree
broker:
  llm_auth: subscription
manager:
  image: scionlocal/lever-claude:latest
  allow_ports: [3305]
workers:
  - name: worker
    dir: workers/worker
`
	p := filepath.Join(dir, "app.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Egress is an explicit posture, decoupled from llm_auth: closed only when
// `egress: closed` is set; api-key alone leaves egress open.
func TestApplyEgressPostureFromConfig(t *testing.T) {
	closedApp := &config.App{Egress: config.EgressClosed, Broker: config.Broker{LLMAuth: config.LLMAuthAPIKey, JailPort: 8443}}
	if !closedApp.ClosedInternetEgress() {
		t.Fatal("egress: closed must resolve closed egress")
	}
	openApp := &config.App{Broker: config.Broker{LLMAuth: config.LLMAuthAPIKey, JailPort: 8443}}
	if openApp.ClosedInternetEgress() {
		t.Fatal("api-key WITHOUT egress: closed must leave egress open (decoupled)")
	}
}

// TestApplyOpenEgressForSubscription verifies that a subscription instance does
// not set the closed posture.
func TestApplyOpenEgressForSubscription(t *testing.T) {
	app := &config.App{Broker: config.Broker{LLMAuth: config.LLMAuthSubscription, JailPort: 8443}}
	if app.ClosedInternetEgress() {
		t.Fatal("subscription instance must not resolve closed egress")
	}
}

func TestApplyDryRun(t *testing.T) {
	p := writeTmpConfig(t)

	// newApplyCmd with nil BackendFactory is safe for --dry-run: the backend
	// is never touched in dry-run mode (plan is printed and the func returns).
	cmd := newApplyCmd(nil)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{p, "--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "jail-up") {
		t.Errorf("dry-run output should contain 'jail-up'; got:\n%s", got)
	}
	if !strings.Contains(got, "start-manager") {
		t.Errorf("dry-run output should contain 'start-manager'; got:\n%s", got)
	}
}

// TestBuildApplyDepsRemoveJailFileRunsThroughJailRunner verifies the argv that
// Deps.RemoveJailFile sends through the jail runner: a `sh -c` guard that
// removes a marker FILE (never a directory) at the given jail-absolute path,
// invoked as `sh -c '<script>' _ <jailPath>` so the removal shares the jail's
// own filesystem view with the `scion init` that follows it (see
// internal/apply/run.go's register-project case for the
// VirtioFS unlink/init race this closes).
func TestBuildApplyDepsRemoveJailFileRunsThroughJailRunner(t *testing.T) {
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	f := leverexec.NewFakeRunner()
	f.Script("sh", leverexec.Result{Stdout: "ok"})
	sb := &stubBackend{runner: f}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }

	deps, _, _, err := buildApplyDeps(context.Background(), app, p, bf, nil)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	if deps.RemoveJailFile == nil {
		t.Fatal("buildApplyDeps did not wire Deps.RemoveJailFile")
	}
	if err := deps.RemoveJailFile(context.Background(), "/lever/.scion"); err != nil {
		t.Fatalf("RemoveJailFile: %v", err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("expected exactly one jail-runner call, got %+v", f.Calls)
	}
	call := f.Calls[0]
	if call.Name != "sh" {
		t.Fatalf("call.Name = %q, want %q", call.Name, "sh")
	}
	if len(call.Args) != 4 || call.Args[0] != "-c" {
		t.Fatalf("call.Args = %+v, want [-c <script> _ /lever/.scion]", call.Args)
	}
	script := call.Args[1]
	if !strings.Contains(script, `[ ! -d "$1" ]`) || !strings.Contains(script, `rm -f -- "$1"`) {
		t.Fatalf("script %q does not guard directories / use $1 for the target", script)
	}
	if call.Args[2] != "_" || call.Args[3] != "/lever/.scion" {
		t.Fatalf("call.Args tail = %+v, want [_ /lever/.scion] (positional $1 via `sh -c script _ path`)", call.Args[2:])
	}
}

// TestBuildApplyDepsWiresRemoveScionProjectConfigs verifies buildApplyDeps
// wires Deps.RemoveScionProjectConfigs straight through to the backend method
// (which itself reaches the guest — see internal/backend/guest/scionstate.go),
// so the register-project step in internal/apply/run.go can
// clear stale ~/.scion/project-configs registrations before `scion init`.
func TestBuildApplyDepsWiresRemoveScionProjectConfigs(t *testing.T) {
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	sb := &stubBackend{}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }

	deps, _, _, err := buildApplyDeps(context.Background(), app, p, bf, nil)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	if deps.RemoveScionProjectConfigs == nil {
		t.Fatal("buildApplyDeps did not wire Deps.RemoveScionProjectConfigs")
	}
	if err := deps.RemoveScionProjectConfigs(context.Background(), "/lever/workers/worker"); err != nil {
		t.Fatalf("RemoveScionProjectConfigs: %v", err)
	}
	if len(sb.removeScionCalls) != 1 || sb.removeScionCalls[0] != "/lever/workers/worker" {
		t.Fatalf("backend.RemoveScionProjectConfigs calls = %+v, want exactly one call with \"/lever/workers/worker\"", sb.removeScionCalls)
	}
}

// TestBuildApplyDepsWiresScionProjectRegistered verifies buildApplyDeps wires
// Deps.ScionProjectRegistered straight through to the backend method, so the
// register-project step (internal/apply/run.go) can observe
// whether its destructive clean+init path is even necessary before running it.
func TestBuildApplyDepsWiresScionProjectRegistered(t *testing.T) {
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	sb := &stubBackend{registeredResult: true}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }

	deps, _, _, err := buildApplyDeps(context.Background(), app, p, bf, nil)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	if deps.ScionProjectRegistered == nil {
		t.Fatal("buildApplyDeps did not wire Deps.ScionProjectRegistered")
	}
	ok, err := deps.ScionProjectRegistered(context.Background(), "/lever/workers/worker")
	if err != nil {
		t.Fatalf("ScionProjectRegistered: %v", err)
	}
	if !ok {
		t.Fatal("expected the stubbed true result to pass through")
	}
	if len(sb.registeredCalls) != 1 || sb.registeredCalls[0] != "/lever/workers/worker" {
		t.Fatalf("backend.ScionProjectRegistered calls = %+v, want exactly one call with \"/lever/workers/worker\"", sb.registeredCalls)
	}
}

// TestBuildApplyDepsWiresEnsureControllerPAT verifies buildApplyDeps wires
// Deps.EnsureControllerPAT to the real ensureControllerPAT (see its doc in
// apply.go), threading through the jail runner, a state dir derived from the
// config path, app.Tree, and the stub backend's MountDest — exercising the
// same mint window as TestEnsureControllerPATMintsThenNoOps, but through the
// buildApplyDeps wiring rather than calling ensureControllerPAT directly.
func TestBuildApplyDepsWiresEnsureControllerPAT(t *testing.T) {
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	f := leverexec.NewFakeRunner()
	f.Script("scion server start", leverexec.Result{})
	f.Script("scion list", leverexec.Result{})
	f.Script("scion init", leverexec.Result{})
	f.Script("scion hub link", leverexec.Result{})
	f.Script("scion hub token create", leverexec.Result{Stdout: "Token: pat-wired-abc\n"})
	f.Script("scion server stop", leverexec.Result{})
	f.Script("sh -c printf", leverexec.Result{Stdout: "/home/tester"}) // $HOME resolution for the dev-token path
	f.Script("sh -c if", leverexec.Result{})                           // the guarded removeJailFile rm
	sb := &stubBackend{runner: f}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }

	deps, _, _, err := buildApplyDeps(context.Background(), app, p, bf, nil)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	if deps.EnsureControllerPAT == nil {
		t.Fatal("buildApplyDeps did not wire Deps.EnsureControllerPAT")
	}
	if err := deps.EnsureControllerPAT(context.Background()); err != nil {
		t.Fatalf("EnsureControllerPAT: %v", err)
	}

	state := brokerctl.StateDir(filepath.Dir(p))
	tok, err := state.LoadControllerPAT()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "pat-wired-abc" {
		t.Fatalf("persisted PAT = %q, want %q (buildApplyDeps' state dir must derive from configPath)", tok, "pat-wired-abc")
	}
}

// TestBuildApplyDepsWiresRearmBootstrap verifies buildApplyDeps wires
// Deps.RearmBootstrap (fix/rearm-bootstrap-on-create — see its doc in
// internal/apply/run.go). RearmBootstrap's real implementation stops+restarts
// the broker and hits its live HTTP admin API, which is not something a unit
// test should exercise (no live broker here — see this branch's CODE-ONLY
// constraint), so this only pins that buildApplyDeps wires a non-nil func;
// the behavior itself is covered by internal/apply's fake-deps tests.
func TestBuildApplyDepsWiresRearmBootstrap(t *testing.T) {
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	sb := &stubBackend{}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }

	deps, _, _, err := buildApplyDeps(context.Background(), app, p, bf, nil)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	if deps.RearmBootstrap == nil {
		t.Fatal("buildApplyDeps did not wire Deps.RearmBootstrap")
	}
}

// TestBuildApplyDepsWiresRemoteProxy mirrors TestBuildApplyDepsWiresRearmBootstrap:
// it only pins that buildApplyDeps wires non-nil StartRemoteProxy/StopRemoteProxy
// funcs. Start's live spawn behavior is covered by the
// TestRemoteControllerStart* tests below; StopRemoteProxy's real kill
// mechanism is covered in internal/brokerctl (it's a thin passthrough here).
func TestBuildApplyDepsWiresRemoteProxy(t *testing.T) {
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	sb := &stubBackend{}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }

	deps, _, _, err := buildApplyDeps(context.Background(), app, p, bf, nil)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	if deps.StartRemoteProxy == nil {
		t.Fatal("buildApplyDeps did not wire Deps.StartRemoteProxy")
	}
	if deps.StopRemoteProxy == nil {
		t.Fatal("buildApplyDeps did not wire Deps.StopRemoteProxy")
	}
}

func TestApplyDryRunDiscoversConfig(t *testing.T) {
	dir := instanceDir(t, "demo")
	t.Chdir(dir)

	cmd := newApplyCmd(nil) // nil backend safe for --dry-run
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--dry-run"}) // NO config arg — discovered from cwd

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "jail-up") || !strings.Contains(got, "start-manager") {
		t.Errorf("dry-run via discovery produced:\n%s", got)
	}
}

func TestBrokerServeCmdIsDetachedAndLogged(t *testing.T) {
	dir := t.TempDir()
	// A non-existent .lever-state subdir mirrors a fresh apply: brokerServeCmd
	// must MkdirAll the log's parent, or the open (and the whole bring-up) fails.
	out := filepath.Join(dir, ".lever-state", "broker.out.log")
	cmd, f, err := brokerServeCmd("/usr/local/bin/lever", "/x/lever.yaml", out, "198.51.100.7", "stephen", "501")
	if err != nil {
		t.Fatalf("brokerServeCmd: %v", err)
	}
	defer f.Close()
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("broker child must be Setsid (own session)")
	}
	if cmd.SysProcAttr.Setpgid {
		t.Fatal("Setpgid should be replaced by Setsid, not both")
	}
	if cmd.Args[len(cmd.Args)-3] != "broker" || cmd.Args[len(cmd.Args)-2] != "serve" {
		t.Fatalf("argv = %v, want ...broker serve <config>", cmd.Args)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("out log not created: %v", err)
	}
	joined := strings.Join(cmd.Env, "\n")
	for _, want := range []string{"LEVER_HOST_ALIAS_IP=198.51.100.7", "LEVER_JAIL_USER=stephen", "LEVER_JAIL_UID=501"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env missing %q", want)
		}
	}
}

func TestRemoteServeCmdIsDetachedAndLogged(t *testing.T) {
	dir := t.TempDir()
	// A non-existent .lever-state subdir mirrors a fresh apply: remoteServeCmd
	// must MkdirAll the log's parent, or the open (and Start) fails.
	out := filepath.Join(dir, ".lever-state", "remote.log")
	cmd, f, err := remoteServeCmd("/usr/local/bin/lever", "/x/lever.yaml", out)
	if err != nil {
		t.Fatalf("remoteServeCmd: %v", err)
	}
	defer f.Close()
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("remote proxy child must be Setsid (own session)")
	}
	if cmd.Args[len(cmd.Args)-3] != "remote" || cmd.Args[len(cmd.Args)-2] != "serve" {
		t.Fatalf("argv = %v, want ...remote serve <config>", cmd.Args)
	}
	if cmd.Args[len(cmd.Args)-1] != "/x/lever.yaml" {
		t.Fatalf("argv config path = %q, want %q", cmd.Args[len(cmd.Args)-1], "/x/lever.yaml")
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("out log not created: %v", err)
	}
}

// TestRemoteControllerStartReusesAlreadyServingProxy proves the idempotence
// shortcut required by the Task 8 contract ("re-apply with a live proxy does
// NOT spawn a second one"): with a live pid recorded (self-signal-0 trick —
// the test process's own pid is always alive, same as
// TestRemoteStatusReportsLivePidAndListener in remote_test.go) AND something
// actually listening on the configured port, Start must not spawn.
// brokerSelfExe is pointed at a nonexistent binary so a WRONGLY-taken spawn
// branch fails loudly (cmd.Start() errors) instead of silently succeeding —
// same "prove the branch, not just the observable" shape as
// TestStartBrokerReusesMatchingBrokerIdentity's directory-as-pidfile trick in
// apply_closures_test.go.
func TestRemoteControllerStartReusesAlreadyServingProxy(t *testing.T) {
	prev := brokerSelfExe
	brokerSelfExe = func() string { return "/no/such/lever-binary" }
	t.Cleanup(func() { brokerSelfExe = prev })

	dir := t.TempDir()
	state := brokerctl.StateDir(dir)
	if err := os.MkdirAll(state.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.RemotePID(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	// The stamp must MATCH, or Start correctly treats the running proxy as
	// serving a different config and restarts it — which here would kill the
	// test process, since remote.pid names it.
	if err := state.WriteRemoteStamp("v-test", "hash-test"); err != nil {
		t.Fatal(err)
	}
	rc := &remoteController{state: state, configPath: "/x/lever.yaml", port: port,
		version: "v-test", cfgHash: "hash-test"}
	if err := rc.Start(context.Background()); err != nil {
		t.Fatalf("Start on an already-serving proxy must reuse (nil), got: %v", err)
	}
}

// TestRemoteControllerStartRestartsOnConfigChange is the defect this stamp
// exists for. The proxy reads `remote:` ONCE and caches it in the handler it
// builds at startup, so a running proxy keeps enforcing the config it was born
// with. Before the stamp, Start returned "already serving" on pid+port alone:
// enabling allowed_users on the live instance left the old process running and
// identity-free requests kept returning 200, while apply reported success.
//
// The running proxy is a real child process here, NOT this test's own pid: the
// mismatch path kills what remote.pid names, and the self-pid trick used above
// would kill the test. brokerSelfExe points at a nonexistent binary so the
// respawn fails loudly — proving Start reached the spawn rather than reusing.
func TestRemoteControllerStartRestartsOnConfigChange(t *testing.T) {
	prev := brokerSelfExe
	brokerSelfExe = func() string { return "/no/such/lever-binary" }
	t.Cleanup(func() { brokerSelfExe = prev })

	dir := t.TempDir()
	state := brokerctl.StateDir(dir)
	if err := os.MkdirAll(state.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := exec.Command("/bin/sh", "-c", "sleep 30")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = victim.Process.Kill(); _, _ = victim.Process.Wait() })
	if err := os.WriteFile(state.RemotePID(), []byte(strconv.Itoa(victim.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Stamp records a DIFFERENT config from the one the controller wants.
	if err := state.WriteRemoteStamp("v-test", "hash-OLD"); err != nil {
		t.Fatal(err)
	}
	rc := &remoteController{state: state, configPath: "/x/lever.yaml",
		port: ln.Addr().(*net.TCPAddr).Port, version: "v-test", cfgHash: "hash-NEW"}

	if err := rc.Start(context.Background()); err == nil {
		t.Fatal("Start reused a proxy running a DIFFERENT remote config — a changed allowed_users/base_url would be silently ignored")
	}
	// The stale proxy must be gone: leaving it alive would keep the old config
	// serving AND hold the port, so the respawn could never bind.
	//
	// Reaped via Wait, not signal(0): a killed-but-unreaped child is a ZOMBIE,
	// and signal 0 succeeds on a zombie because the process entry still exists
	// — so the obvious check passes whether or not the kill worked.
	done := make(chan struct{})
	go func() { _, _ = victim.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("the stale proxy was left running — the respawn could never bind the port")
	}
}

// TestRemoteControllerStartRespawnsStalePID proves the second idempotence
// case: "re-apply after a killed proxy respawns it". A pid file naming a
// process that is definitely not running (the doctor checks' own
// implausibly-high-pid convention — see TestCheckBrokerAliveStalePID) must
// fall through to a fresh spawn, not a false "already serving".
// brokerSelfExe points at `true` so the spawn succeeds harmlessly (exits 0
// immediately, no lingering process) — mirrors
// buildDepsAgainstFakeBroker's broker-spawn tests in apply_closures_test.go.
//
// That stand-in never listens, so Start's post-spawn liveness wait
// legitimately fails: what this asserts is the SPAWN DECISION, and the
// not-listening error is the proof the spawn happened and was then checked.
// (A nil here would mean the liveness check had been lost.)
func TestRemoteControllerStartRespawnsStalePID(t *testing.T) {
	prev := brokerSelfExe
	brokerSelfExe = func() string { return "/usr/bin/true" }
	t.Cleanup(func() { brokerSelfExe = prev })
	shortenRemoteProxyStartWait(t)

	dir := t.TempDir()
	state := brokerctl.StateDir(dir)
	if err := os.MkdirAll(state.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.RemotePID(), []byte("2147483646\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rc := &remoteController{state: state, configPath: "/x/lever.yaml", port: 48997}
	err := rc.Start(context.Background())
	if err == nil {
		t.Fatal("a stand-in that never listens must not report the proxy as serving")
	}
	if !strings.Contains(err.Error(), "not listening") {
		t.Fatalf("Start after a killed proxy must respawn and then check it, got: %v", err)
	}
	if _, serr := os.Stat(state.RemoteLog()); serr != nil {
		t.Fatalf("remote.log not created by the respawn: %v", serr)
	}
}

// shortenRemoteProxyStartWait keeps the spawn tests fast: they use a stand-in
// that never binds, so they always pay the full wait.
func shortenRemoteProxyStartWait(t *testing.T) {
	t.Helper()
	timeout, interval := remoteProxyStartTimeout, remoteProxyStartInterval
	t.Cleanup(func() { remoteProxyStartTimeout, remoteProxyStartInterval = timeout, interval })
	remoteProxyStartTimeout, remoteProxyStartInterval = 50*time.Millisecond, 5*time.Millisecond
}

// TestRemoteControllerStartSpawnsWhenNeverStarted covers the third
// precondition: no remote.pid at all (a fresh instance, or remote access
// just enabled) must spawn, exactly like the stale-pid case.
func TestRemoteControllerStartSpawnsWhenNeverStarted(t *testing.T) {
	prev := brokerSelfExe
	brokerSelfExe = func() string { return "/usr/bin/true" }
	t.Cleanup(func() { brokerSelfExe = prev })
	shortenRemoteProxyStartWait(t)

	dir := t.TempDir()
	state := brokerctl.StateDir(dir) // no remote.pid at all

	rc := &remoteController{state: state, configPath: "/x/lever.yaml", port: 48996}
	// As above: the stand-in never binds, so the spawn is proved by the
	// liveness check's complaint rather than by a nil.
	err := rc.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not listening") {
		t.Fatalf("Start with no prior proxy must spawn and then check it, got: %v", err)
	}
	if _, serr := os.Stat(state.RemoteLog()); serr != nil {
		t.Fatalf("remote.log not created by the spawn: %v", serr)
	}
}

// callIndex returns the index of the first call in calls satisfying pred, or
// -1 if none matches. Helper for the ordered-call assertions below.
func callIndex(calls []leverexec.Call, pred func(leverexec.Call) bool) int {
	for i, c := range calls {
		if pred(c) {
			return i
		}
	}
	return -1
}

func callHasPrefix(c leverexec.Call, prefix string) bool {
	full := strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
	return strings.HasPrefix(full, prefix)
}

// TestEnsureControllerPATMintsThenNoOps drives the whole mint window (see
// ensureControllerPAT's doc in apply.go) against a fake runner: the first call
// must run the throwaway server start → init → hub link → hub token create
// (exact scopes, no agent:message) → persist 0600 → stop → best-effort
// dev-token removal, IN THAT ORDER; a second call, with the PAT now
// persisted, must be a complete no-op (no new runner calls at all — in
// particular no second throwaway server start).
func TestEnsureControllerPATMintsThenNoOps(t *testing.T) {
	tree := t.TempDir()
	state := brokerctl.StateDir(t.TempDir())
	const jailMount = "/lever"

	f := leverexec.NewFakeRunner()
	f.Script("scion server start", leverexec.Result{})
	f.Script("scion list", leverexec.Result{}) // waitHubReady's poll, run inside ServerStart
	f.Script("scion init", leverexec.Result{})
	f.Script("scion hub link", leverexec.Result{})
	f.Script("scion hub token create", leverexec.Result{Stdout: "Token: pat-mint-xyz\n"})
	f.Script("scion server stop", leverexec.Result{})
	f.Script("sh -c printf", leverexec.Result{Stdout: "/home/tester"}) // $HOME resolution for the dev-token path
	f.Script("sh -c if", leverexec.Result{})                           // the guarded removeJailFile rm

	if err := ensureControllerPAT(context.Background(), f, state, tree, jailMount, false); err != nil {
		t.Fatalf("ensureControllerPAT: %v", err)
	}

	// Persisted 0600 with the minted token.
	fi, err := os.Stat(state.ControllerPAT())
	if err != nil {
		t.Fatalf("controller.pat not written: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("controller.pat perm = %#o, want 0600", perm)
	}
	tok, err := state.LoadControllerPAT()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "pat-mint-xyz" {
		t.Fatalf("persisted PAT = %q, want %q", tok, "pat-mint-xyz")
	}

	iStart := callIndex(f.Calls, func(c leverexec.Call) bool { return callHasPrefix(c, "scion server start") })
	iInit := callIndex(f.Calls, func(c leverexec.Call) bool { return callHasPrefix(c, "scion init") })
	iLink := callIndex(f.Calls, func(c leverexec.Call) bool { return callHasPrefix(c, "scion hub link") })
	iToken := callIndex(f.Calls, func(c leverexec.Call) bool { return callHasPrefix(c, "scion hub token create") })
	iStop := callIndex(f.Calls, func(c leverexec.Call) bool { return callHasPrefix(c, "scion server stop") })
	iRm := callIndex(f.Calls, func(c leverexec.Call) bool { return callHasPrefix(c, "sh -c if") })
	if iStart < 0 || iInit < 0 || iLink < 0 || iToken < 0 || iStop < 0 || iRm < 0 {
		t.Fatalf("missing expected call(s); calls=%+v", f.Calls)
	}
	if !(iStart < iInit && iInit < iLink && iLink < iToken && iToken < iStop) {
		t.Fatalf("calls out of order: start=%d init=%d link=%d token=%d stop=%d", iStart, iInit, iLink, iToken, iStop)
	}

	// Fixed throwaway port, distinct from the real hub's 8080; dev-auth ON.
	startArgs := strings.Join(f.Calls[iStart].Args, " ")
	if !strings.Contains(startArgs, "--web-port 48080") || !strings.Contains(startArgs, "--dev-auth=true") {
		t.Fatalf("throwaway server start args = %q, want --web-port 48080 --dev-auth=true", startArgs)
	}

	// init/hub-link run inside the jail project dir (the tree root).
	if f.Calls[iInit].Dir != jailMount {
		t.Fatalf("init dir = %q, want %q", f.Calls[iInit].Dir, jailMount)
	}
	if f.Calls[iLink].Dir != jailMount {
		t.Fatalf("hub link dir = %q, want %q", f.Calls[iLink].Dir, jailMount)
	}

	// scion requires --project (name/ID) and --name; project name is the jail
	// mount's basename ("lever"). Exact scopes string — no agent:message (every
	// interactive verb, message included, gates on agent:attach; see the P3 plan).
	tokenArgs := strings.Join(f.Calls[iToken].Args, " ")
	if !strings.Contains(tokenArgs, "--project lever") {
		t.Fatalf("hub token create args = %q, want --project lever", tokenArgs)
	}
	if !strings.Contains(tokenArgs, "--name lever-controller") {
		t.Fatalf("hub token create args = %q, want --name lever-controller", tokenArgs)
	}
	if !strings.Contains(tokenArgs, "--scopes agent:manage,agent:attach,project:read") {
		t.Fatalf("hub token create args = %q, want --scopes agent:manage,agent:attach,project:read", tokenArgs)
	}
	// Token create runs in the project dir so scion resolves the project context.
	if f.Calls[iToken].Dir != jailMount {
		t.Fatalf("hub token create dir = %q, want %q", f.Calls[iToken].Dir, jailMount)
	}

	// Best-effort dev-token removal ran through the guarded removeJailFile helper
	// (removeJailFileScript) against ~/.scion/dev-token, where ~ was resolved
	// in-jail to the scripted $HOME (/home/tester) — not a hardcoded home.
	rm := f.Calls[iRm]
	if len(rm.Args) != 4 || rm.Args[0] != "-c" || rm.Args[2] != "_" || rm.Args[3] != "/home/tester/.scion/dev-token" {
		t.Fatalf("dev-token removal args = %+v, want [-c <script> _ /home/tester/.scion/dev-token]", rm.Args)
	}

	callsAfterFirst := len(f.Calls)

	// Second call: PAT already persisted → no-op. In particular, no second
	// throwaway server start (the agent-free mint window opens at most once).
	if err := ensureControllerPAT(context.Background(), f, state, tree, jailMount, false); err != nil {
		t.Fatalf("second ensureControllerPAT: %v", err)
	}
	if len(f.Calls) != callsAfterFirst {
		t.Fatalf("second call made %d new runner call(s), want 0 (must be a no-op): %+v",
			len(f.Calls)-callsAfterFirst, f.Calls[callsAfterFirst:])
	}
}

// scriptPATMintChain registers the throwaway-window call chain shared by every
// ensureControllerPAT test below (server start → list poll → init → hub link →
// server stop → dev-token resolve/rm), everything except the "hub token
// create" calls themselves — those differ per test by --name/token, so each
// test scripts them individually via scriptTokenCreate.
func scriptPATMintChain(f *leverexec.FakeRunner) {
	f.Script("scion server start", leverexec.Result{})
	f.Script("scion list", leverexec.Result{}) // waitHubReady's poll, run inside ServerStart
	f.Script("scion init", leverexec.Result{})
	f.Script("scion hub link", leverexec.Result{})
	f.Script("scion server stop", leverexec.Result{})
	f.Script("sh -c printf", leverexec.Result{Stdout: "/home/tester"}) // $HOME resolution for the dev-token path
	f.Script("sh -c if", leverexec.Result{})                           // the guarded removeJailFile rm
}

// scriptTokenCreate registers a distinct "hub token create --project lever
// --name <name>" response so the fake runner can tell the controller and
// remote mints apart (they differ only by --name, which lands right after
// --project in the argv scion.Client.HubTokenCreate builds).
func scriptTokenCreate(f *leverexec.FakeRunner, name, token string) {
	f.Script("scion hub token create --project lever --name "+name, leverexec.Result{Stdout: "Token: " + token + "\n"})
}

// countCalls returns how many recorded calls satisfy pred.
func countCalls(calls []leverexec.Call, pred func(leverexec.Call) bool) int {
	n := 0
	for _, c := range calls {
		if pred(c) {
			n++
		}
	}
	return n
}

// tokenCreateCallFor returns the "scion hub token create" call whose --name
// flag equals name, so a test can inspect ITS --scopes argv directly rather
// than assuming which of possibly several token-create calls is which.
func tokenCreateCallFor(t *testing.T, calls []leverexec.Call, name string) leverexec.Call {
	t.Helper()
	for _, c := range calls {
		if !callHasPrefix(c, "scion hub token create") {
			continue
		}
		for i, a := range c.Args {
			if a == "--name" && i+1 < len(c.Args) && c.Args[i+1] == name {
				return c
			}
		}
	}
	t.Fatalf("no hub token create call with --name %s found; calls=%+v", name, calls)
	return leverexec.Call{}
}

// scopesArg returns the literal value of a hub-token-create call's --scopes
// flag (the exact argv element, not a substring of the joined command line —
// a Contains check on the joined string passes even when extra scopes are
// appended, since the expected prefix is still present).
func scopesArg(t *testing.T, c leverexec.Call) string {
	t.Helper()
	for i, a := range c.Args {
		if a == "--scopes" && i+1 < len(c.Args) {
			return c.Args[i+1]
		}
	}
	t.Fatalf("no --scopes flag in call args=%+v", c.Args)
	return ""
}

// TestEnsurePATsMintsBothInOneWindow: fresh state, remoteEnabled=true — both
// the controller and remote PATs are missing, so ONE throwaway dev-auth
// window mints both (see ensureControllerPAT's doc: minting both in one
// window preserves the agent-free-window property on first bootstrap).
func TestEnsurePATsMintsBothInOneWindow(t *testing.T) {
	tree := t.TempDir()
	state := brokerctl.StateDir(t.TempDir())
	const jailMount = "/lever"

	f := leverexec.NewFakeRunner()
	scriptPATMintChain(f)
	scriptTokenCreate(f, "lever-controller", "pat-controller-1")
	scriptTokenCreate(f, "lever-remote", "pat-remote-1")

	if err := ensureControllerPAT(context.Background(), f, state, tree, jailMount, true); err != nil {
		t.Fatalf("ensureControllerPAT: %v", err)
	}

	if n := countCalls(f.Calls, func(c leverexec.Call) bool { return callHasPrefix(c, "scion server start") }); n != 1 {
		t.Fatalf("scion server start calls = %d, want 1 (one shared window)", n)
	}
	// The throwaway mint hub must never gain the web flags even though
	// remoteEnabled is true here: EnableWeb/BaseURL are for the REAL,
	// dev-auth-OFF hub the scion-server apply step starts right after this
	// one (internal/apply/run.go) — this hub is a dev-auth-ON, agent-free
	// bootstrap window that must stay unreachable/no-SPA regardless of the
	// remote-access setting.
	for _, c := range f.Calls {
		if callHasPrefix(c, "scion server start") {
			joined := strings.Join(c.Args, " ")
			if strings.Contains(joined, "--enable-web") || strings.Contains(joined, "--base-url") {
				t.Fatalf("throwaway mint hub must not carry web flags: %q", joined)
			}
		}
	}
	// ServerStop is called both explicitly (post-mint cleanup) and via the
	// deferred kill (belt-and-braces against a partial start) — see
	// ensureControllerPAT's doc; TestEnsureControllerPATMintsThenNoOps checks
	// existence for the same reason, not an exact count.
	if n := countCalls(f.Calls, func(c leverexec.Call) bool { return callHasPrefix(c, "scion server stop") }); n < 1 {
		t.Fatalf("scion server stop calls = %d, want at least 1", n)
	}
	if n := countCalls(f.Calls, func(c leverexec.Call) bool { return callHasPrefix(c, "scion hub token create") }); n != 2 {
		t.Fatalf("scion hub token create calls = %d, want 2 (controller + remote)", n)
	}

	// Exact-match the minted scopes (not a Contains/prefix check — that would
	// silently pass even if an extra scope, e.g. agent:manage, were appended
	// to remotePATScopes; see remotePATScopes's doc for why that must never
	// happen for the remote-proxy token).
	controllerCall := tokenCreateCallFor(t, f.Calls, "lever-controller")
	if got, want := scopesArg(t, controllerCall), "agent:manage,agent:attach,project:read,project:update"; got != want {
		t.Fatalf("controller mint --scopes = %q, want %q", got, want)
	}
	remoteCall := tokenCreateCallFor(t, f.Calls, "lever-remote")
	if got, want := scopesArg(t, remoteCall), "agent:read,agent:list,project:read,agent:attach"; got != want {
		t.Fatalf("remote mint --scopes = %q, want %q", got, want)
	}

	ctok, err := state.LoadControllerPAT()
	if err != nil {
		t.Fatal(err)
	}
	if ctok != "pat-controller-1" {
		t.Fatalf("persisted controller PAT = %q, want %q", ctok, "pat-controller-1")
	}
	rtok, err := state.LoadRemotePAT()
	if err != nil {
		t.Fatal(err)
	}
	if rtok != "pat-remote-1" {
		t.Fatalf("persisted remote PAT = %q, want %q", rtok, "pat-remote-1")
	}
	for _, path := range []string{state.ControllerPAT(), state.RemotePAT()} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s not written: %v", path, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s perm = %#o, want 0600", path, perm)
		}
	}
}

// TestEnsurePATsRemoteOnlyWindowWhenControllerExists: the controller PAT was
// already minted (a pre-existing instance), remoteEnabled=true and the remote
// PAT is missing (remote.enabled flipped on later). A window still opens —
// this is the "later-enable" repair case documented alongside the
// controller-PAT re-mint shape — but it mints ONLY the remote token; the
// controller PAT is left untouched.
func TestEnsurePATsRemoteOnlyWindowWhenControllerExists(t *testing.T) {
	tree := t.TempDir()
	state := brokerctl.StateDir(t.TempDir())
	const jailMount = "/lever"
	if err := state.SaveControllerPAT("pat-controller-existing"); err != nil {
		t.Fatal(err)
	}

	f := leverexec.NewFakeRunner()
	scriptPATMintChain(f)
	scriptTokenCreate(f, "lever-remote", "pat-remote-2")
	// Deliberately NOT scripting "--name lever-controller": if the code
	// mistakenly re-mints the controller PAT, the fake runner errors on the
	// unscripted command and fails the test.

	if err := ensureControllerPAT(context.Background(), f, state, tree, jailMount, true); err != nil {
		t.Fatalf("ensureControllerPAT: %v", err)
	}

	if n := countCalls(f.Calls, func(c leverexec.Call) bool { return callHasPrefix(c, "scion server start") }); n != 1 {
		t.Fatalf("scion server start calls = %d, want 1 (remote-only window still opens)", n)
	}
	if n := countCalls(f.Calls, func(c leverexec.Call) bool { return callHasPrefix(c, "scion hub token create") }); n != 1 {
		t.Fatalf("scion hub token create calls = %d, want 1 (remote only)", n)
	}

	ctok, err := state.LoadControllerPAT()
	if err != nil {
		t.Fatal(err)
	}
	if ctok != "pat-controller-existing" {
		t.Fatalf("controller PAT = %q, want unchanged %q", ctok, "pat-controller-existing")
	}
	rtok, err := state.LoadRemotePAT()
	if err != nil {
		t.Fatal(err)
	}
	if rtok != "pat-remote-2" {
		t.Fatalf("persisted remote PAT = %q, want %q", rtok, "pat-remote-2")
	}
}

// TestEnsurePATsNoWindowWhenNothingMissing: both PATs already persisted —
// nothing to mint, so no dev-auth window opens at all (zero scion calls).
func TestEnsurePATsNoWindowWhenNothingMissing(t *testing.T) {
	tree := t.TempDir()
	state := brokerctl.StateDir(t.TempDir())
	const jailMount = "/lever"
	if err := state.SaveControllerPAT("pat-controller-existing"); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveRemotePAT("pat-remote-existing"); err != nil {
		t.Fatal(err)
	}

	f := leverexec.NewFakeRunner() // no scripts: any call is an error

	if err := ensureControllerPAT(context.Background(), f, state, tree, jailMount, true); err != nil {
		t.Fatalf("ensureControllerPAT: %v", err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("scion calls = %d, want 0 (nothing missing, no window): %+v", len(f.Calls), f.Calls)
	}
}

// TestEnsurePATsRemoteDisabledUnchanged: remoteEnabled=false on a fresh
// state reproduces the exact pre-remote behavior — one controller mint, no
// lever-remote token, no remote.pat written.
func TestEnsurePATsRemoteDisabledUnchanged(t *testing.T) {
	tree := t.TempDir()
	state := brokerctl.StateDir(t.TempDir())
	const jailMount = "/lever"

	f := leverexec.NewFakeRunner()
	scriptPATMintChain(f)
	scriptTokenCreate(f, "lever-controller", "pat-controller-3")
	// Deliberately NOT scripting "--name lever-remote": remoteEnabled=false
	// must never touch it.

	if err := ensureControllerPAT(context.Background(), f, state, tree, jailMount, false); err != nil {
		t.Fatalf("ensureControllerPAT: %v", err)
	}

	if n := countCalls(f.Calls, func(c leverexec.Call) bool { return callHasPrefix(c, "scion hub token create") }); n != 1 {
		t.Fatalf("scion hub token create calls = %d, want 1 (controller only)", n)
	}
	ctok, err := state.LoadControllerPAT()
	if err != nil {
		t.Fatal(err)
	}
	if ctok != "pat-controller-3" {
		t.Fatalf("persisted controller PAT = %q, want %q", ctok, "pat-controller-3")
	}
	rtok, err := state.LoadRemotePAT()
	if err != nil {
		t.Fatal(err)
	}
	if rtok != "" {
		t.Fatalf("remote PAT = %q, want empty (remote disabled, never minted)", rtok)
	}
}

// TestApplyBootstrapTokenThenLockedHubEndToEnd is Task 6's end-to-end proof:
// it drives the REAL ensureControllerPAT (behind Deps.EnsureControllerPAT, as
// wired by buildApplyDeps) followed by the REAL-hub scion-server start —
// exactly the "bootstrap-token" then "scion-server" step sequence runStep
// executes for every apply (internal/apply/run.go) — through the SAME
// scion.Client object apply.Run would drive, over one fake jail runner and
// one temp .lever-state dir.
//
// This is deliberately NOT a re-test of TestEnsureControllerPATMintsThenNoOps
// (the mint window in isolation) or TestBuildApplyDepsWiresEnsureControllerPAT
// (wiring only, no scion-server). Its new value is proving the COMPOSITION:
// bootstrap-token precedes scion-server; scion-server locks the real hub
// (port 8080, dev-auth off); and the mint→persist→thread round-trip actually
// closes the loop — the client that starts the locked hub carries
// SCION_HUB_TOKEN=<the PAT ensureControllerPAT just minted and persisted> in
// its env, because HubTokenSource reads state.LoadControllerPAT() lazily
// (see scion.Options.HubTokenSource's doc). A second, fresh buildApplyDeps
// (a re-apply against the same config/state dir) must skip the throwaway
// entirely (PAT already persisted) while still threading the SAME PAT into
// the reused hub's env.
func TestApplyBootstrapTokenThenLockedHubEndToEnd(t *testing.T) {
	ctx := context.Background()
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}

	f := leverexec.NewFakeRunner()
	f.Script("scion server start", leverexec.Result{})
	f.Script("scion list", leverexec.Result{}) // waitHubReady's poll (throwaway AND real hub)
	f.Script("scion init", leverexec.Result{})
	f.Script("scion hub link", leverexec.Result{})
	f.Script("scion hub token create", leverexec.Result{Stdout: "Token: pat-e2e-round-trip\n"})
	f.Script("scion server stop", leverexec.Result{})
	f.Script("sh -c printf", leverexec.Result{Stdout: "/home/tester"}) // $HOME resolution for the dev-token path
	f.Script("sh -c if", leverexec.Result{})                           // the guarded removeJailFile rm
	sb := &stubBackend{runner: f}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }

	// --- First apply: bootstrap-token then scion-server, via the real Deps
	// wiring (mirrors runStep's "bootstrap-token"/"scion-server" arms).
	deps, _, _, err := buildApplyDeps(ctx, app, p, bf, nil)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	if deps.EnsureControllerPAT == nil {
		t.Fatal("buildApplyDeps did not wire Deps.EnsureControllerPAT")
	}
	if err := deps.EnsureControllerPAT(ctx); err != nil {
		t.Fatalf("bootstrap-token step: %v", err)
	}
	if err := deps.Scion.ServerStart(ctx, scion.ServerOpts{WebPort: 8080, DevAuth: false}); err != nil {
		t.Fatalf("scion-server step: %v", err)
	}

	// bootstrap-token precedes scion-server: the throwaway (48080, dev-auth
	// ON) server start must land BEFORE the real hub's (8080, dev-auth OFF).
	iThrowaway := callIndex(f.Calls, func(c leverexec.Call) bool {
		return callHasPrefix(c, "scion server start --web-port 48080")
	})
	iReal := callIndex(f.Calls, func(c leverexec.Call) bool {
		return callHasPrefix(c, "scion server start --web-port 8080")
	})
	if iThrowaway < 0 || iReal < 0 {
		t.Fatalf("missing server-start call(s); calls=%+v", f.Calls)
	}
	if !(iThrowaway < iReal) {
		t.Fatalf("throwaway server start (call %d) must precede the real hub server start (call %d)", iThrowaway, iReal)
	}

	// scion-server locks the real hub: port 8080, dev-auth off.
	realArgs := strings.Join(f.Calls[iReal].Args, " ")
	if !strings.Contains(realArgs, "--web-port 8080") || !strings.Contains(realArgs, "--dev-auth=false") {
		t.Fatalf("real hub server start args = %q, want --web-port 8080 --dev-auth=false", realArgs)
	}

	// The mint → persist → thread round-trip: the SAME client that started
	// the real, dev-auth-off hub carries SCION_HUB_TOKEN=<minted PAT> in the
	// env it sent the runner for that call.
	if got := f.Calls[iReal].Env["SCION_HUB_TOKEN"]; got != "pat-e2e-round-trip" {
		t.Fatalf("real hub server-start env SCION_HUB_TOKEN = %q, want %q (mint->thread round-trip broken)", got, "pat-e2e-round-trip")
	}

	// Persisted 0600 under the config-derived state dir.
	state := brokerctl.StateDir(filepath.Dir(p))
	fi, err := os.Stat(state.ControllerPAT())
	if err != nil {
		t.Fatalf("controller.pat not written: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("controller.pat perm = %#o, want 0600", perm)
	}

	callsAfterFirst := len(f.Calls)

	// --- Second apply: a FRESH buildApplyDeps against the same config/state
	// dir (what a real re-apply invocation does). The PAT is already
	// persisted, so bootstrap-token must be a complete no-op — in
	// particular, no second throwaway server start.
	deps2, _, _, err := buildApplyDeps(ctx, app, p, bf, nil)
	if err != nil {
		t.Fatalf("buildApplyDeps (2nd apply): %v", err)
	}
	if err := deps2.EnsureControllerPAT(ctx); err != nil {
		t.Fatalf("bootstrap-token step (2nd apply): %v", err)
	}
	if len(f.Calls) != callsAfterFirst {
		t.Fatalf("2nd apply's bootstrap-token made %d new runner call(s), want 0 (must be a no-op): %+v",
			len(f.Calls)-callsAfterFirst, f.Calls[callsAfterFirst:])
	}

	// scion-server still runs on every apply (locking the hub is not itself
	// gated on the mint) and must thread the SAME reused PAT.
	if err := deps2.Scion.ServerStart(ctx, scion.ServerOpts{WebPort: 8080, DevAuth: false}); err != nil {
		t.Fatalf("scion-server step (2nd apply): %v", err)
	}
	// The 2nd apply's real hub server-start is the LAST such call (ServerStart
	// itself appends a trailing waitHubReady "list" call right after it, so
	// it is not simply the last entry in f.Calls).
	iReal2 := -1
	for idx, c := range f.Calls {
		if callHasPrefix(c, "scion server start --web-port 8080") {
			iReal2 = idx
		}
	}
	if iReal2 < callsAfterFirst {
		t.Fatalf("2nd apply's real hub server start not found after call %d; calls=%+v", callsAfterFirst, f.Calls)
	}
	if got := f.Calls[iReal2].Env["SCION_HUB_TOKEN"]; got != "pat-e2e-round-trip" {
		t.Fatalf("2nd apply's real hub server-start env SCION_HUB_TOKEN = %q, want %q (reused PAT not threaded)", got, "pat-e2e-round-trip")
	}
}

// TestRemoteProxyStartFailsLoudlyWhenItNeverBinds is the regression test for a
// silent success: a proxy that died on a deterministic bind error (its port
// already taken — which OrbStack's mirroring of a guest listener can do) left
// `lever apply` printing "is up" and exiting 0, with the operator's next
// signal a 502 in a browser.
func TestRemoteProxyStartFailsLoudlyWhenItNeverBinds(t *testing.T) {
	savedTimeout, savedInterval := remoteProxyStartTimeout, remoteProxyStartInterval
	t.Cleanup(func() { remoteProxyStartTimeout, remoteProxyStartInterval = savedTimeout, savedInterval })
	remoteProxyStartTimeout, remoteProxyStartInterval = 50*time.Millisecond, 5*time.Millisecond

	dir := t.TempDir()
	state := brokerctl.StateDir(dir)
	if err := os.MkdirAll(state.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// What the proxy actually wrote in the live failure.
	logLine := "Error: remoteproxy: bind: listen tcp 127.0.0.1:8446: bind: address already in use"
	if err := os.WriteFile(state.RemoteLog(), []byte("remote proxy \"x\" serving\n"+logLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A port nothing is listening on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	rc := &remoteController{state: state, configPath: filepath.Join(dir, "lever.yaml"), port: port}
	err = rc.awaitListening()
	if err == nil {
		t.Fatal("a proxy that never bound was reported as started")
	}
	// The cause lives only in that log — this process never sees the child's
	// stderr — so the error has to carry it.
	if !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("error does not quote the log's reason: %v", err)
	}
	if !strings.Contains(err.Error(), state.RemoteLog()) {
		t.Fatalf("error does not name the log: %v", err)
	}

	// And a proxy that IS listening passes.
	ln2, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("could not re-bind %d: %v", port, err)
	}
	defer func() { _ = ln2.Close() }()
	if err := rc.awaitListening(); err != nil {
		t.Fatalf("a listening proxy must pass: %v", err)
	}
}

func TestLastLogLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remote.log")
	if got := lastLogLine(filepath.Join(dir, "absent.log")); got != "" {
		t.Fatalf("absent file = %q, want empty", got)
	}
	if err := os.WriteFile(path, []byte("first\nlast line\n\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastLogLine(path); got != "last line" {
		t.Fatalf("lastLogLine = %q, want the final non-empty line", got)
	}
}
