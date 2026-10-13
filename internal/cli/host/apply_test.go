package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/testutil"
)

// writeTmpConfig writes a "demo" instance declaring the worker "worker" with a
// real tree/workers/worker subdir.
func writeTmpConfig(t *testing.T) string {
	t.Helper()
	dir := writeInstance(t, "manager:\n  image: scionlocal/lever-claude:latest\n  allow_ports: [3305]\nworkers:\n  - name: worker\n    dir: workers/worker\n")
	if err := os.MkdirAll(filepath.Join(dir, "workspace", "workers", "worker"), 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, config.CanonicalName)
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

func TestApplyOpenEgressForSubscription(t *testing.T) {
	app := &config.App{Broker: config.Broker{LLMAuth: config.LLMAuthSubscription, JailPort: 8443}}
	if app.ClosedInternetEgress() {
		t.Fatal("subscription instance must not resolve closed egress")
	}
}

func TestApplyDryRun(t *testing.T) {
	p := writeTmpConfig(t)

	// A nil BackendFactory is safe for --dry-run: the backend is never touched.
	cmd := newApplyCmd(nil)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{p, "--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "broker-up") {
		t.Errorf("dry-run output should contain 'broker-up'; got:\n%s", got)
	}
	if !strings.Contains(got, "start-manager") {
		t.Errorf("dry-run output should contain 'start-manager'; got:\n%s", got)
	}
}

// TestBuildApplyDepsRemoveJailFileRunsThroughJailRunner pins the argv: a
// `sh -c '<script>' _ <jailPath>` guard that removes a file, never a
// directory, through the jail runner.
func TestBuildApplyDepsRemoveJailFileRunsThroughJailRunner(t *testing.T) {
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	f := proc.NewFakeRunner()
	f.Script("sh", proc.Result{Stdout: "ok"})
	sb := &stubBackend{runner: f}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }

	w, err := buildApplyDeps(context.Background(), app, p, bf, inertApplyOpts)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	deps := w.deps
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

// TestBuildApplyDepsEnsuresSessionSecret: buildApplyDeps ensures the session
// secret (0600, 64 hex) and threads it into Deps.HubSessionSecret; a second
// build adopts the same value (rotation is deleting the file). Failure
// messages show length and equality only, never the secret.
func TestBuildApplyDepsEnsuresSessionSecret(t *testing.T) {
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	sb := &stubBackend{}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }

	w, err := buildApplyDeps(context.Background(), app, p, bf, inertApplyOpts)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	deps := w.deps
	if len(deps.HubSessionSecret) != 64 {
		t.Fatalf("Deps.HubSessionSecret length = %d, want 64 (32 bytes hex)", len(deps.HubSessionSecret))
	}
	st := stateFor(p)
	fi, err := os.Stat(st.SessionSecret())
	if err != nil {
		t.Fatalf("session-secret not persisted: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("session-secret perms = %#o, want 0600", perm)
	}
	onDisk, err := st.EnsureSessionSecret()
	if err != nil {
		t.Fatal(err)
	}
	if onDisk != deps.HubSessionSecret {
		t.Fatal("Deps.HubSessionSecret differs from the persisted file")
	}

	w2, err := buildApplyDeps(context.Background(), app, p, bf, inertApplyOpts)
	if err != nil {
		t.Fatalf("second buildApplyDeps: %v", err)
	}
	deps2 := w2.deps
	if deps2.HubSessionSecret != deps.HubSessionSecret {
		t.Fatal("re-apply changed the session secret — must adopt, not rotate")
	}
}

func TestBuildApplyDepsWiresRemoveScionProjectConfigs(t *testing.T) {
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	sb := &stubBackend{}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }

	w, err := buildApplyDeps(context.Background(), app, p, bf, inertApplyOpts)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	deps := w.deps
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

func TestBuildApplyDepsWiresScionProjectRegistered(t *testing.T) {
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	sb := &stubBackend{registeredResult: true}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }

	w, err := buildApplyDeps(context.Background(), app, p, bf, inertApplyOpts)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	deps := w.deps
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

// TestBuildApplyDepsWiresEnsureControllerPAT runs the mint window through the
// buildApplyDeps wiring rather than calling ensureControllerPAT directly.
func TestBuildApplyDepsWiresEnsureControllerPAT(t *testing.T) {
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	f := patMintRunner("pat-wired-abc")
	sb := &stubBackend{runner: f}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }

	w, err := buildApplyDeps(context.Background(), app, p, bf, inertApplyOpts)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	deps := w.deps
	if deps.EnsureControllerPAT == nil {
		t.Fatal("buildApplyDeps did not wire Deps.EnsureControllerPAT")
	}
	if err := deps.EnsureControllerPAT(context.Background()); err != nil {
		t.Fatalf("EnsureControllerPAT: %v", err)
	}

	st := state.ForConfig(filepath.Dir(p))
	tok, err := st.LoadControllerPAT()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "pat-wired-abc" {
		t.Fatalf("persisted PAT = %q, want %q (buildApplyDeps' state dir must derive from configPath)", tok, "pat-wired-abc")
	}
}

// TestBuildApplyDepsWiresRearmBootstrap only pins that the func is wired: the
// real one needs a live broker, and the behaviour is covered by
// internal/apply's fake-deps tests.
func TestBuildApplyDepsWiresRearmBootstrap(t *testing.T) {
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	sb := &stubBackend{}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }

	w, err := buildApplyDeps(context.Background(), app, p, bf, inertApplyOpts)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	deps := w.deps
	if deps.RearmBootstrap == nil {
		t.Fatal("buildApplyDeps did not wire Deps.RearmBootstrap")
	}
}

// TestBuildApplyDepsWiresRemoteProxy only pins that the funcs are wired; Start
// is covered by the TestRemoteControllerStart* tests and Stop in
// internal/brokerctl.
func TestBuildApplyDepsWiresRemoteProxy(t *testing.T) {
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	sb := &stubBackend{}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }

	w, err := buildApplyDeps(context.Background(), app, p, bf, inertApplyOpts)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	deps := w.deps
	if deps.StartRemoteProxy == nil {
		t.Fatal("buildApplyDeps did not wire Deps.StartRemoteProxy")
	}
	if deps.StopRemoteProxy == nil {
		t.Fatal("buildApplyDeps did not wire Deps.StopRemoteProxy")
	}
}

func TestApplyDryRunDiscoversConfig(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	cmd := newApplyCmd(nil) // nil backend safe for --dry-run
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--dry-run"}) // NO config arg — discovered from cwd

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "broker-up") || !strings.Contains(got, "start-manager") {
		t.Errorf("dry-run via discovery produced:\n%s", got)
	}
}

func TestBrokerServeCmdIsDetachedAndLogged(t *testing.T) {
	dir := t.TempDir()
	// A missing .lever-state mirrors a fresh apply: the log's parent must be
	// created.
	out := filepath.Join(dir, ".lever-state", "broker.out.log")
	cmd, f, err := brokerServeCmd("/usr/local/bin/lever", "/x/lever.yaml", out, "198.51.100.7", "devuser", "501")
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
	for _, want := range []string{"LEVER_HOST_ALIAS_IP=198.51.100.7", "LEVER_JAIL_USER=devuser", "LEVER_JAIL_UID=501"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env missing %q", want)
		}
	}
}

func TestRemoteServeCmdIsDetachedAndLogged(t *testing.T) {
	dir := t.TempDir()
	// A missing .lever-state mirrors a fresh apply: the log's parent must be
	// created.
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

// TestRemoteControllerStartReusesAlreadyServingProxy: with a live, listening,
// correctly stamped proxy Start must not spawn. selfExe does not exist, so a
// wrongly taken spawn branch fails loudly.
func TestRemoteControllerStartReusesAlreadyServingProxy(t *testing.T) {
	selfExe := "/no/such/lever-binary"

	dir := t.TempDir()
	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// remote.pid must name a `lever remote serve` of this instance, not merely a
	// live process.
	proxy, proxyDone := testutil.StartServeProcess(t, state.RemoteDaemon, filepath.Join(dir, "lever.yaml"))
	if err := os.WriteFile(st.RemotePID(), []byte(strconv.Itoa(proxy.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	if err := st.WriteRemoteStamp("v-test", "hash-test"); err != nil {
		t.Fatal(err)
	}
	rc := &remoteController{state: st, configPath: "/x/lever.yaml", port: port,
		version: "v-test", cfgHash: "hash-test", selfExe: selfExe}
	if err := rc.Start(context.Background()); err != nil {
		t.Fatalf("Start on an already-serving proxy must reuse (nil), got: %v", err)
	}
	select {
	case <-proxyDone:
		t.Fatal("the reused proxy was stopped")
	default:
	}
}

// remote.pid names a live process that is not this instance's proxy (a reused
// pid): Start neither reuses nor signals it, and spawns (failing here, since
// selfExe does not exist).
func TestRemoteControllerStartTreatsAReusedPIDAsStale(t *testing.T) {
	dir := t.TempDir()
	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.RemotePID(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// A mismatched stamp: stopping a pid taken for the proxy would kill this test.
	if err := st.WriteRemoteStamp("v-test", "hash-OLD"); err != nil {
		t.Fatal(err)
	}
	rc := &remoteController{state: st, configPath: "/x/lever.yaml", port: ln.Addr().(*net.TCPAddr).Port,
		version: "v-test", cfgHash: "hash-NEW", selfExe: "/no/such/lever-binary"}
	if err := rc.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "/no/such/lever-binary") {
		t.Fatalf("Start = %v, want the spawn's failure", err)
	}
}

// TestRemoteControllerStartRestartsOnConfigChange: the proxy caches `remote:`
// at startup, so a stamp mismatch must restart it, or a change such as
// enabling allowed_users silently does nothing.
//
// The running proxy is a real child, not this test's pid, since the mismatch
// path kills what remote.pid names. selfExe does not exist, so the respawn
// fails loudly.
func TestRemoteControllerStartRestartsOnConfigChange(t *testing.T) {
	selfExe := "/no/such/lever-binary"

	dir := t.TempDir()
	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim, victimDone := testutil.StartServeProcess(t, state.RemoteDaemon, filepath.Join(dir, "lever.yaml"))
	if err := os.WriteFile(st.RemotePID(), []byte(strconv.Itoa(victim.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if err := st.WriteRemoteStamp("v-test", "hash-OLD"); err != nil {
		t.Fatal(err)
	}
	rc := &remoteController{state: st, configPath: "/x/lever.yaml",
		port: ln.Addr().(*net.TCPAddr).Port, version: "v-test", cfgHash: "hash-NEW", selfExe: selfExe}

	if err := rc.Start(context.Background()); err == nil {
		t.Fatal("Start reused a proxy running a DIFFERENT remote config — a changed allowed_users/base_url would be silently ignored")
	}
	// The stale proxy must be gone, or it keeps the old config and the port.
	// Checked via Wait, not signal 0: signal 0 succeeds on an unreaped zombie.
	select {
	case <-victimDone:
	case <-time.After(5 * time.Second):
		t.Error("the stale proxy was left running — the respawn could never bind the port")
	}
}

// TestRemoteControllerStartRespawnsStalePID: a pid file naming a dead process
// must lead to a spawn, not a false "already serving". The stand-in (`true`)
// never listens, so the not-listening error proves the spawn happened and was
// then checked.
func TestRemoteControllerStartRespawnsStalePID(t *testing.T) {
	selfExe := "/usr/bin/true"

	dir := t.TempDir()
	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.RemotePID(), []byte("2147483646\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rc := shortenRemoteProxyStartWait(&remoteController{state: st, configPath: filepath.Join(dir, "lever.yaml"), port: 48997, selfExe: selfExe})
	err := rc.Start(context.Background())
	if err == nil {
		t.Fatal("a stand-in that never listens must not report the proxy as serving")
	}
	testutil.WantErrIs(t, err, errRemoteProxyNotListening) // respawned, then checked
	if _, serr := os.Stat(st.RemoteLog()); serr != nil {
		t.Fatalf("remote.log not created by the respawn: %v", serr)
	}
}

// The spawn tests' stand-in never binds, so they always pay the full wait.
func shortenRemoteProxyStartWait(rc *remoteController) *remoteController {
	rc.startTimeout, rc.startInterval = 50*time.Millisecond, 5*time.Millisecond
	return rc
}

// TestRemoteControllerStartSpawnsWhenNeverStarted: no remote.pid at all must
// spawn.
func TestRemoteControllerStartSpawnsWhenNeverStarted(t *testing.T) {
	selfExe := "/usr/bin/true"

	dir := t.TempDir()
	st := state.ForConfig(dir) // no remote.pid at all

	rc := shortenRemoteProxyStartWait(&remoteController{state: st, configPath: filepath.Join(dir, "lever.yaml"), port: 48996, selfExe: selfExe})
	err := rc.Start(context.Background())
	testutil.WantErrIs(t, err, errRemoteProxyNotListening) // spawned, then checked
	if _, serr := os.Stat(st.RemoteLog()); serr != nil {
		t.Fatalf("remote.log not created by the spawn: %v", serr)
	}
}

// callIndex returns the index of the first call satisfying pred, or -1.
func callIndex(calls []proc.Call, pred func(proc.Call) bool) int {
	for i, c := range calls {
		if pred(c) {
			return i
		}
	}
	return -1
}

func callHasPrefix(c proc.Call, prefix string) bool {
	full := strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
	return strings.HasPrefix(full, prefix)
}

// TestEnsureControllerPATMintsThenNoOps: the first call runs the throwaway
// start, init, hub link, token create with exact scopes, persist 0600, stop and
// dev-token removal in that order; a second call with the PAT persisted makes
// no runner calls at all.
func TestEnsureControllerPATMintsThenNoOps(t *testing.T) {
	tree := t.TempDir()
	st := state.ForConfig(t.TempDir())
	const jailMount = "/lever"

	f := patMintRunner("pat-mint-xyz")

	if err := ensureControllerPAT(context.Background(), f, st, tree, jailMount, remoteAccess{}, patMintOpts{}); err != nil {
		t.Fatalf("ensureControllerPAT: %v", err)
	}

	fi, err := os.Stat(st.ControllerPAT())
	if err != nil {
		t.Fatalf("controller.pat not written: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("controller.pat perm = %#o, want 0600", perm)
	}
	tok, err := st.LoadControllerPAT()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "pat-mint-xyz" {
		t.Fatalf("persisted PAT = %q, want %q", tok, "pat-mint-xyz")
	}

	iStart := callIndex(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, "scion server start") })
	iInit := callIndex(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, "scion init") })
	iLink := callIndex(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, "scion hub link") })
	iToken := callIndex(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, "scion hub token create") })
	// Two stops: the first clears the shared pid file before the start, the second
	// stops the throwaway after the mint.
	var stops []int
	for i, c := range f.Calls {
		if callHasPrefix(c, "scion server stop") {
			stops = append(stops, i)
		}
	}
	iRm := callIndex(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, "sh -c if") })
	if iStart < 0 || iInit < 0 || iLink < 0 || iToken < 0 || len(stops) != 2 || iRm < 0 {
		t.Fatalf("missing expected call(s); calls=%+v", f.Calls)
	}
	if !(stops[0] < iStart && iStart < iInit && iInit < iLink && iLink < iToken && iToken < stops[1]) {
		t.Fatalf("calls out of order: prestop=%d start=%d init=%d link=%d token=%d stop=%d", stops[0], iStart, iInit, iLink, iToken, stops[1])
	}

	// Throwaway port distinct from 8080; dev-auth on; web off, so no dev
	// auto-login and the API binds --port; runtime broker off, so the dev-auth hub
	// cannot create full-role agents.
	startArgs := strings.Join(f.Calls[iStart].Args, " ")
	const wantStart = "server start --port 48080 --enable-web=false --enable-runtime-broker=false --dev-auth=true"
	if startArgs != wantStart {
		t.Fatalf("throwaway server start args = %q, want %q", startArgs, wantStart)
	}
	// The container check runs before the live hub is stopped.
	if iPs := callIndex(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, argvPodmanPs) }); iPs < 0 || iPs > stops[0] {
		t.Fatalf("container check at %d, want before the first stop at %d", iPs, stops[0])
	}

	if f.Calls[iInit].Dir != jailMount {
		t.Fatalf("init dir = %q, want %q", f.Calls[iInit].Dir, jailMount)
	}
	if f.Calls[iLink].Dir != jailMount {
		t.Fatalf("hub link dir = %q, want %q", f.Calls[iLink].Dir, jailMount)
	}

	// The project name is the jail mount's basename ("lever"). The scope set is
	// exact; newer scions gate message sends on agent:message alone.
	tokenArgs := strings.Join(f.Calls[iToken].Args, " ")
	if !strings.Contains(tokenArgs, "--project lever") {
		t.Fatalf("hub token create args = %q, want --project lever", tokenArgs)
	}
	if !strings.Contains(tokenArgs, "--name lever-controller") {
		t.Fatalf("hub token create args = %q, want --name lever-controller", tokenArgs)
	}
	if got, want := scopesArg(t, f.Calls[iToken]), strings.Join(controllerPATScopes(false, false), ","); got != want {
		t.Fatalf("hub token create --scopes = %q, want %q", got, want)
	}
	// Never scion's 90-day default: an expiring token takes the instance down.
	if !strings.Contains(tokenArgs, "--expires "+patExpires) {
		t.Fatalf("hub token create args = %q, want --expires %s", tokenArgs, patExpires)
	}
	if f.Calls[iToken].Dir != jailMount {
		t.Fatalf("hub token create dir = %q, want %q", f.Calls[iToken].Dir, jailMount)
	}

	// The dev-token removal uses removeJailFileScript on ~/.scion/dev-token, with
	// ~ resolved in-jail, not hardcoded.
	rm := f.Calls[iRm]
	if len(rm.Args) != 4 || rm.Args[0] != "-c" || rm.Args[2] != "_" || rm.Args[3] != "/home/tester/.scion/dev-token" {
		t.Fatalf("dev-token removal args = %+v, want [-c <script> _ /home/tester/.scion/dev-token]", rm.Args)
	}

	callsAfterFirst := len(f.Calls)

	// Second call: no-op; the window opens at most once.
	if err := ensureControllerPAT(context.Background(), f, st, tree, jailMount, remoteAccess{}, patMintOpts{}); err != nil {
		t.Fatalf("second ensureControllerPAT: %v", err)
	}
	if len(f.Calls) != callsAfterFirst {
		t.Fatalf("second call made %d new runner call(s), want 0 (must be a no-op): %+v",
			len(f.Calls)-callsAfterFirst, f.Calls[callsAfterFirst:])
	}
}

func countCalls(calls []proc.Call, pred func(proc.Call) bool) int {
	n := 0
	for _, c := range calls {
		if pred(c) {
			n++
		}
	}
	return n
}

// tokenCreateCallFor returns the token-create call whose --name is name.
func tokenCreateCallFor(t *testing.T, calls []proc.Call, name string) proc.Call {
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
	return proc.Call{}
}

// scopesArg returns the exact --scopes argv element: a Contains check on the
// joined line would pass with extra scopes appended.
func scopesArg(t *testing.T, c proc.Call) string {
	t.Helper()
	for i, a := range c.Args {
		if a == "--scopes" && i+1 < len(c.Args) {
			return c.Args[i+1]
		}
	}
	t.Fatalf("no --scopes flag in call args=%+v", c.Args)
	return ""
}

// TestEnsurePATsMintsBothInOneWindow: a fresh state with remote enabled mints
// both PATs in one window.
func TestEnsurePATsMintsBothInOneWindow(t *testing.T) {
	tree := t.TempDir()
	st := state.ForConfig(t.TempDir())
	const jailMount = "/lever"

	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	scriptTokenCreate(f, "lever-controller", "pat-controller-1")
	scriptTokenCreate(f, "lever-remote", "pat-remote-1")

	if err := ensureControllerPAT(context.Background(), f, st, tree, jailMount, remoteAccess{Enabled: true}, patMintOpts{AdminHub: newFakeAdminHub()}); err != nil {
		t.Fatalf("ensureControllerPAT: %v", err)
	}

	if n := countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, "scion server start") }); n != 1 {
		t.Fatalf("scion server start calls = %d, want 1 (one shared window)", n)
	}
	// The throwaway must keep the web frontend off even with remote enabled: a
	// dev-auth hub with the frontend on signs any session-less request in as
	// super-admin.
	for _, c := range f.Calls {
		if callHasPrefix(c, "scion server start") {
			joined := strings.Join(c.Args, " ")
			if !strings.Contains(joined, "--enable-web=false") || strings.Contains(joined, "--web-assets-dir") || strings.Contains(joined, "--base-url") {
				t.Fatalf("throwaway mint hub must run with --enable-web=false and no web flags: %q", joined)
			}
		}
	}
	// ServerStop runs explicitly and via the deferred kill, so check existence,
	// not an exact count.
	if n := countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, "scion server stop") }); n < 1 {
		t.Fatalf("scion server stop calls = %d, want at least 1", n)
	}
	if n := countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, "scion hub token create") }); n != 2 {
		t.Fatalf("scion hub token create calls = %d, want 2 (controller + remote)", n)
	}

	// Exact match: an extra scope such as agent:manage must never reach the
	// remote token.
	controllerCall := tokenCreateCallFor(t, f.Calls, "lever-controller")
	if got, want := scopesArg(t, controllerCall), strings.Join(controllerPATScopes(false, false), ","); got != want {
		t.Fatalf("controller mint --scopes = %q, want %q", got, want)
	}
	remoteCall := tokenCreateCallFor(t, f.Calls, "lever-remote")
	if got, want := scopesArg(t, remoteCall), "agent:read,agent:list,project:read,agent:attach,agent:message"; got != want {
		t.Fatalf("remote mint --scopes = %q, want %q", got, want)
	}

	ctok, err := st.LoadControllerPAT()
	if err != nil {
		t.Fatal(err)
	}
	if ctok != "pat-controller-1" {
		t.Fatalf("persisted controller PAT = %q, want %q", ctok, "pat-controller-1")
	}
	rtok, err := st.LoadRemotePAT()
	if err != nil {
		t.Fatal(err)
	}
	if rtok != "pat-remote-1" {
		t.Fatalf("persisted remote PAT = %q, want %q", rtok, "pat-remote-1")
	}
	for _, path := range []string{st.ControllerPAT(), st.RemotePAT()} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s not written: %v", path, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s perm = %#o, want 0600", path, perm)
		}
	}
}

// TestEnsurePATsRemoteOnlyWindowWhenControllerExists: remote access enabled
// later opens a window that mints only the remote token.
func TestEnsurePATsRemoteOnlyWindowWhenControllerExists(t *testing.T) {
	tree := t.TempDir()
	st := state.ForConfig(t.TempDir())
	const jailMount = "/lever"
	seedPAT(t, st, "controller", "pat-controller-existing")

	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	scriptTokenCreate(f, "lever-remote", "pat-remote-2")
	// "--name lever-controller" is unscripted, so a controller re-mint fails the
	// test.

	if err := ensureControllerPAT(context.Background(), f, st, tree, jailMount, remoteAccess{Enabled: true}, patMintOpts{AdminHub: newFakeAdminHub()}); err != nil {
		t.Fatalf("ensureControllerPAT: %v", err)
	}

	if n := countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, "scion server start") }); n != 1 {
		t.Fatalf("scion server start calls = %d, want 1 (remote-only window still opens)", n)
	}
	if n := countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, "scion hub token create") }); n != 1 {
		t.Fatalf("scion hub token create calls = %d, want 1 (remote only)", n)
	}

	ctok, err := st.LoadControllerPAT()
	if err != nil {
		t.Fatal(err)
	}
	if ctok != "pat-controller-existing" {
		t.Fatalf("controller PAT = %q, want unchanged %q", ctok, "pat-controller-existing")
	}
	rtok, err := st.LoadRemotePAT()
	if err != nil {
		t.Fatal(err)
	}
	if rtok != "pat-remote-2" {
		t.Fatalf("persisted remote PAT = %q, want %q", rtok, "pat-remote-2")
	}
}

// TestEnsurePATsNoWindowWhenNothingMissing: with both PATs and the role grant
// recorded, no window opens (zero scion calls).
func TestEnsurePATsNoWindowWhenNothingMissing(t *testing.T) {
	tree := t.TempDir()
	st := state.ForConfig(t.TempDir())
	const jailMount = "/lever"
	seedPAT(t, st, "controller", "pat-controller-existing")
	seedPAT(t, st, "remote", "pat-remote-existing")
	if err := st.SaveRemoteRoleRecord(state.RemoteRoleRecord{Permissions: remoteRolePermissions(false),
		Bound:              map[string]string{"lever-operator@lever.local": "user-1"},
		Ceilings:           map[string]string{"lever-operator@lever.local": "ac-1"},
		CeilingPermissions: remoteRolePermissions(false)}); err != nil {
		t.Fatal(err)
	}

	f := proc.NewFakeRunner() // no scripts: any call is an error

	ra := remoteAccess{Enabled: true, Emails: []string{"lever-operator@lever.local"}}
	if err := ensureControllerPAT(context.Background(), f, st, tree, jailMount, ra, patMintOpts{}); err != nil {
		t.Fatalf("ensureControllerPAT: %v", err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("scion calls = %d, want 0 (nothing missing, no window): %+v", len(f.Calls), f.Calls)
	}
}

// TestEnsurePATsRemoteDisabledUnchanged: with remote off, one controller mint
// and no remote token.
func TestEnsurePATsRemoteDisabledUnchanged(t *testing.T) {
	tree := t.TempDir()
	st := state.ForConfig(t.TempDir())
	const jailMount = "/lever"

	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	scriptTokenCreate(f, "lever-controller", "pat-controller-3")
	// "--name lever-remote" is unscripted: remote off must never mint it.

	if err := ensureControllerPAT(context.Background(), f, st, tree, jailMount, remoteAccess{}, patMintOpts{}); err != nil {
		t.Fatalf("ensureControllerPAT: %v", err)
	}

	if n := countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, "scion hub token create") }); n != 1 {
		t.Fatalf("scion hub token create calls = %d, want 1 (controller only)", n)
	}
	ctok, err := st.LoadControllerPAT()
	if err != nil {
		t.Fatal(err)
	}
	if ctok != "pat-controller-3" {
		t.Fatalf("persisted controller PAT = %q, want %q", ctok, "pat-controller-3")
	}
	rtok, err := st.LoadRemotePAT()
	if err != nil {
		t.Fatal(err)
	}
	if rtok != "" {
		t.Fatalf("remote PAT = %q, want empty (remote disabled, never minted)", rtok)
	}
}

// TestApplyBootstrapTokenThenLockedHubEndToEnd drives the real
// ensureControllerPAT and then the real-hub server start through the same
// scion.Client apply.Run uses. It proves the composition: the throwaway runs
// first; the real hub runs on 8080 with dev-auth off and carries the just-minted
// PAT as SCION_HUB_TOKEN (HubTokenSource reads it lazily); a re-apply skips the
// throwaway and threads the same PAT.
func TestApplyBootstrapTokenThenLockedHubEndToEnd(t *testing.T) {
	ctx := context.Background()
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}

	f := patMintRunner("pat-e2e-round-trip")
	sb := &stubBackend{runner: f}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }

	// First apply: bootstrap-token, then scion-server.
	w, err := buildApplyDeps(ctx, app, p, bf, inertApplyOpts)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	deps := w.deps
	if deps.EnsureControllerPAT == nil {
		t.Fatal("buildApplyDeps did not wire Deps.EnsureControllerPAT")
	}
	if err := deps.EnsureControllerPAT(ctx); err != nil {
		t.Fatalf("bootstrap-token step: %v", err)
	}
	if err := deps.Scion.ServerStart(ctx, scion.ServerOpts{WebPort: 8080, DevAuth: false}); err != nil {
		t.Fatalf("scion-server step: %v", err)
	}

	iThrowaway := callIndex(f.Calls, func(c proc.Call) bool {
		return callHasPrefix(c, "scion server start --port 48080")
	})
	iReal := callIndex(f.Calls, func(c proc.Call) bool {
		return callHasPrefix(c, "scion server start --web-port 8080")
	})
	if iThrowaway < 0 || iReal < 0 {
		t.Fatalf("missing server-start call(s); calls=%+v", f.Calls)
	}
	if !(iThrowaway < iReal) {
		t.Fatalf("throwaway server start (call %d) must precede the real hub server start (call %d)", iThrowaway, iReal)
	}

	realArgs := strings.Join(f.Calls[iReal].Args, " ")
	if !strings.Contains(realArgs, "--web-port 8080") || !strings.Contains(realArgs, "--dev-auth=false") {
		t.Fatalf("real hub server start args = %q, want --web-port 8080 --dev-auth=false", realArgs)
	}

	// The client that started the real hub carries the minted PAT in its env.
	if got := f.Calls[iReal].Env["SCION_HUB_TOKEN"]; got != "pat-e2e-round-trip" {
		t.Fatalf("real hub server-start env SCION_HUB_TOKEN = %q, want %q (mint->thread round-trip broken)", got, "pat-e2e-round-trip")
	}

	st := state.ForConfig(filepath.Dir(p))
	fi, err := os.Stat(st.ControllerPAT())
	if err != nil {
		t.Fatalf("controller.pat not written: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("controller.pat perm = %#o, want 0600", perm)
	}

	callsAfterFirst := len(f.Calls)

	// Second apply against the same state: bootstrap-token must not open a window.
	w2, err := buildApplyDeps(ctx, app, p, bf, inertApplyOpts)
	if err != nil {
		t.Fatalf("buildApplyDeps (2nd apply): %v", err)
	}
	deps2 := w2.deps
	if err := deps2.EnsureControllerPAT(ctx); err != nil {
		t.Fatalf("bootstrap-token step (2nd apply): %v", err)
	}
	// The only call allowed is the read-only scope probe (`scion hub token create
	// --help`).
	for _, c := range f.Calls[callsAfterFirst:] {
		if !callHasPrefix(c, "scion hub token create --help") {
			t.Fatalf("2nd apply's bootstrap-token made runner call %+v, want only the scope probe (must be a no-op): %+v",
				c, f.Calls[callsAfterFirst:])
		}
	}

	if err := deps2.Scion.ServerStart(ctx, scion.ServerOpts{WebPort: 8080, DevAuth: false}); err != nil {
		t.Fatalf("scion-server step (2nd apply): %v", err)
	}
	// ServerStart appends a waitHubReady "list" call, so the server start is not
	// simply the last call.
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

// TestRemoteProxyStartFailsLoudlyWhenItNeverBinds: a proxy that died on a bind
// error (a taken port, which OrbStack's mirroring of a guest listener can
// cause) must fail the apply, not report "is up".
func TestRemoteProxyStartFailsLoudlyWhenItNeverBinds(t *testing.T) {
	dir := t.TempDir()
	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// What the proxy actually wrote in the live failure.
	logLine := "Error: remoteproxy: bind: listen tcp 127.0.0.1:8446: bind: address already in use"
	if err := os.WriteFile(st.RemoteLog(), []byte("remote proxy \"x\" serving\n"+logLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	// The stand-in must outlive the test: awaitListening gives up once the child
	// is gone.
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })

	rc := shortenRemoteProxyStartWait(&remoteController{state: st, configPath: filepath.Join(dir, "lever.yaml"), port: port})
	err = rc.awaitListening(context.Background(), child)
	if err == nil {
		t.Fatal("a proxy that never bound was reported as started")
	}
	// This process never sees the child's stderr, so the error carries the log.
	testutil.WantErrIs(t, err, errRemoteProxyNotListening)
	testutil.WantErrContaining(t, err, "address already in use", st.RemoteLog()) // the log's reason, and the log

	ln2, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("could not re-bind %d: %v", port, err)
	}
	defer func() { _ = ln2.Close() }()
	if err := rc.awaitListening(context.Background(), child); err != nil {
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

// TestRemoteControllerStartStopsOldProxyOnPortChange: a changed remote.port
// makes the dial fail, and the stop must still happen, or the old proxy keeps
// serving the old port with the old config while the respawn overwrites
// remote.pid and leaves it unstoppable.
func TestRemoteControllerStartStopsOldProxyOnPortChange(t *testing.T) {
	selfExe := "/no/such/lever-binary"

	dir := t.TempDir()
	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldProxy, oldProxyDone := testutil.StartServeProcess(t, state.RemoteDaemon, filepath.Join(dir, "lever.yaml"))
	if err := os.WriteFile(st.RemotePID(), []byte(strconv.Itoa(oldProxy.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The stamp matches: a pure port move.
	if err := st.WriteRemoteStamp("v-test", "hash-test"); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freePort := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	rc := &remoteController{state: st, configPath: "/x/lever.yaml", port: freePort,
		version: "v-test", cfgHash: "hash-test", selfExe: selfExe}
	if err := rc.Start(context.Background()); err == nil {
		t.Fatal("Start must not silently succeed here")
	}

	select {
	case <-oldProxyDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the old proxy was LEAKED: still serving the previous port with the previous config, and remote.pid no longer names it")
	}
	// The stop must also drop remote.pid, so nothing later mistakes a process lever
	// no longer controls for the live proxy.
	if _, err := os.Stat(st.RemotePID()); err == nil {
		t.Fatal("remote.pid still names the stopped proxy — `lever remote status` and the next apply both read it as the live one")
	}
}

// TestDetachedSelfCmd pins the daemon spawn shape: argv is self, args and the
// absolute config path; the instance root is the working directory; Setsid;
// env appended; and the out log created and wired to both streams.
func TestDetachedSelfCmd(t *testing.T) {
	out := filepath.Join(t.TempDir(), "state", "x.log")
	cmd, f, err := detachedSelfCmd("/usr/local/bin/lever", out, "/x/lever.yaml", []string{"K=v"}, "remote", "serve")
	if err != nil {
		t.Fatalf("detachedSelfCmd: %v", err)
	}
	defer f.Close()
	if got := strings.Join(cmd.Args, " "); got != "/usr/local/bin/lever remote serve /x/lever.yaml" {
		t.Fatalf("argv = %q", got)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("child must be Setsid")
	}
	if cmd.Dir != "/x" {
		t.Fatalf("child dir = %q, want the instance root /x (supervised tools inherit it)", cmd.Dir)
	}
	if !slices.Contains(cmd.Env, "K=v") || len(cmd.Env) != len(os.Environ())+1 {
		t.Fatalf("env must be the parent's plus K=v, got %d entries", len(cmd.Env))
	}
	if cmd.Stdout != f || cmd.Stderr != f {
		t.Fatal("stdout and stderr must both go to the out log")
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("out log not created: %v", err)
	}
}

// A relative config path (lever apply ../lever.yaml from inside the tree)
// reaches the child absolute, and the child runs in the instance root, not
// in the directory apply was run from.
func TestDetachedSelfCmdRelativeConfigRunsInInstanceRoot(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "ws")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(tree)
	cmd, f, err := brokerServeCmd("/usr/local/bin/lever", "../lever.yaml", filepath.Join(t.TempDir(), "out.log"), "", "", "")
	if err != nil {
		t.Fatalf("brokerServeCmd: %v", err)
	}
	defer f.Close()
	if cmd.Dir != root {
		t.Fatalf("child dir = %q, want the instance root %q", cmd.Dir, root)
	}
	if got := cmd.Args[len(cmd.Args)-1]; got != filepath.Join(root, "lever.yaml") {
		t.Fatalf("config arg = %q, want it absolute", got)
	}
}

// TestRemoteRecordStampWaitsForThePIDFile: the proxy binds its port before
// it writes remote.pid, so apply can see it listening first. The stamp must
// wait for the new proxy's pid file instead of failing to read it.
func TestRemoteRecordStampWaitsForThePIDFile(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const pid = 4242
	rc := &remoteController{state: st, version: "v-test", cfgHash: "hash-test",
		startTimeout: 2 * time.Second, startInterval: 10 * time.Millisecond}
	if err := os.WriteFile(st.RemotePID(), []byte("4241\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		wrote <- os.WriteFile(st.RemotePID(), []byte(strconv.Itoa(pid)+"\n"), 0o600)
	}()
	if err := rc.recordStamp(context.Background(), pid); err != nil {
		t.Fatalf("recordStamp: %v", err)
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if !st.RemoteStampMatches("v-test", "hash-test") {
		t.Fatal("no stamp for the new proxy")
	}
}

// TestRemoteRecordStampTimesOut: a proxy that never writes its pid file
// gets no stamp, and the error says why (apply prints it as a warning).
func TestRemoteRecordStampTimesOut(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rc := &remoteController{state: st, version: "v-test", cfgHash: "hash-test",
		startTimeout: 50 * time.Millisecond, startInterval: 5 * time.Millisecond}
	err := rc.recordStamp(context.Background(), 4242)
	testutil.WantErrContaining(t, err, "remote.pid")
	if _, serr := os.Stat(st.RemoteStamp()); serr == nil {
		t.Fatal("a stamp was written for a proxy that never wrote its pid file")
	}
}

// TestAwaitListeningHonoursContext: a cancelled apply must not keep polling
// for the proxy's bind.
func TestAwaitListeningHonoursContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rc := &remoteController{state: state.ForConfig(t.TempDir()), port: port,
		startTimeout: time.Minute, startInterval: time.Second}
	start := time.Now()
	if err := rc.awaitListening(ctx, child); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("awaitListening kept waiting after the context was cancelled")
	}
}

// recordMounts fails for an absent record, so the check says nothing rather
// than "no mounts".
func TestRecordMounts(t *testing.T) {
	agents := []hubapi.Agent{
		{Slug: "scratch", Volumes: []hubapi.Volume{{Source: "/x", Target: "/y"}}},
		{Slug: "hello", Volumes: []hubapi.Volume{
			{Source: "/lever/kb", Target: "/workspace/kb", ReadOnly: true},
			{Source: "/lever/a", Target: "/workspace/a"},
		}},
	}
	got, err := recordMounts(agents, "hello")
	want := []jail.Mount{{Source: "/lever/kb", Destination: "/workspace/kb"}, {Source: "/lever/a", Destination: "/workspace/a", RW: true}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := recordMounts(agents, "nobody"); err == nil {
		t.Fatal("an absent record must be an error")
	}
}

// TestAgentRevoked: the apply heal's revocation check reads the broker's
// persisted list by cert CN (the manager's slug maps to its CN) and answers
// revoked when the list cannot be read.
func TestAgentRevoked(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	app, err := config.Load(filepath.Join(dir, config.CanonicalName))
	if err != nil {
		t.Fatal(err)
	}
	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	revoked := agentRevoked(app, st)
	if revoked(app.Name) || revoked("scratch") {
		t.Fatal("no list, no revocation")
	}
	if err := brokerctl.SaveRevocation(st, broker.RevocationState{Revoked: []string{app.ManagerCN(), "scratch"}}); err != nil {
		t.Fatal(err)
	}
	if !revoked(app.Name) || !revoked("scratch") || revoked("other") {
		t.Fatal("revoked CNs must read as revoked, by the manager's slug too")
	}
	if err := os.WriteFile(st.Revocation(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !revoked("other") {
		t.Fatal("an unreadable list must fail closed")
	}
}
