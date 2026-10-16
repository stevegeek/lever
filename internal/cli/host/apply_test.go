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

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
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
	before := len(f.Calls) // the guest clock read
	if err := deps.RemoveJailFile(context.Background(), "/lever/.scion"); err != nil {
		t.Fatalf("RemoveJailFile: %v", err)
	}
	f.Calls = f.Calls[before:]
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

// buildApplyDeps creates the hub assertion key under the instance's state
// dir, reads the guest clock through the jail runner, and wires the
// hub-identity deps; EnsureHubAuth hands the backend this instance's key,
// issuer and audience.
func TestBuildApplyDepsWiresHubAuth(t *testing.T) {
	p := writeTmpConfig(t)
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	f := proc.NewFakeRunner()
	f.Script("date +%s", proc.Result{Stdout: "1700000000\n"})
	sb := &stubBackend{runner: f}
	bf := func(string, string) (backend.Backend, error) { return sb, nil }

	w, err := buildApplyDeps(context.Background(), app, p, bf, inertApplyOpts)
	if err != nil {
		t.Fatalf("buildApplyDeps: %v", err)
	}
	st := state.ForConfig(filepath.Dir(p))
	fi, err := os.Stat(st.HubAuthKey())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("hub auth key: %v, %v; want a 0600 file under the state dir", fi, err)
	}
	if countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, "date +%s") }) != 1 {
		t.Fatalf("the guest clock was not read once through the jail runner: %+v", f.Calls)
	}
	if off := w.clock.Offset; off > -time.Hour*24*365 && off < time.Hour*24*365 {
		// 1700000000 is late 2023: a clock read from it is far behind today.
		t.Fatalf("clock offset %v does not come from the guest reading", off)
	}
	d := w.deps
	if d.EnsureHubIdentity == nil || d.EnsureHubAuth == nil || d.ProjectLinked == nil {
		t.Fatal("buildApplyDeps did not wire the hub-identity deps")
	}
	id, err := st.EnsureInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.HubAuthEnv, apply.HubAuthEnv(id, w.hubKey.Fingerprint())) {
		t.Fatalf("HubAuthEnv = %v", d.HubAuthEnv)
	}
	if _, err := d.EnsureHubAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sb.hubAuthCalls) != 1 {
		t.Fatalf("EnsureHubAuth calls = %d, want 1", len(sb.hubAuthCalls))
	}
	got := sb.hubAuthCalls[0]
	if !bytes.Equal(got.PublicKeyPEM, w.hubKey.PublicPEM()) || got.Issuer != hubauth.Issuer(id) ||
		got.Audience != hubauth.Audience(id) || got.AdminEmail != hubauth.AdminEmail || got.Header != hubauth.Header ||
		got.RemoteOn != app.RemoteEnabled() {
		t.Fatalf("EnsureHubAuth spec = %+v", got)
	}

	// A second build keeps the key.
	fp := w.hubKey.Fingerprint()
	w2, err := buildApplyDeps(context.Background(), app, p, bf, inertApplyOpts)
	if err != nil {
		t.Fatal(err)
	}
	if w2.hubKey.Fingerprint() != fp {
		t.Fatal("a re-apply rotated the hub assertion key")
	}
}

// An unreadable guest clock signs on the host clock, with a warning.
func TestReadGuestClockFallsBackToTheHostClock(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("date +%s", proc.Result{Stdout: "not a number\n"})
	var logged []string
	c := readGuestClock(context.Background(), f, func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	if c.Offset != 0 || len(logged) != 1 || !strings.Contains(logged[0], "host clock") {
		t.Fatalf("clock %+v, log %q", c, logged)
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
	if deps.FenceRemoteLogins == nil {
		t.Fatal("buildApplyDeps did not wire Deps.FenceRemoteLogins")
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
		version: "v-test", cfgHash: func() string { return "hash-test" }, selfExe: selfExe, stateInTree: outsideTree}
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
		version: "v-test", cfgHash: func() string { return "hash-NEW" }, selfExe: "/no/such/lever-binary"}
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
		port: ln.Addr().(*net.TCPAddr).Port, version: "v-test", cfgHash: func() string { return "hash-NEW" }, selfExe: selfExe}

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

	rc := shortenRemoteProxyStartWait(&remoteController{state: st, configPath: filepath.Join(dir, "lever.yaml"), port: 48997, selfExe: selfExe, cfgHash: func() string { return "hash-test" }})
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

	rc := shortenRemoteProxyStartWait(&remoteController{state: st, configPath: filepath.Join(dir, "lever.yaml"), port: 48996, selfExe: selfExe, cfgHash: func() string { return "hash-test" }})
	err := rc.Start(context.Background())
	testutil.WantErrIs(t, err, errRemoteProxyNotListening) // spawned, then checked
	if _, serr := os.Stat(st.RemoteLog()); serr != nil {
		t.Fatalf("remote.log not created by the spawn: %v", serr)
	}
}

func callHasPrefix(c proc.Call, prefix string) bool {
	full := strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
	return strings.HasPrefix(full, prefix)
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

	rc := shortenRemoteProxyStartWait(&remoteController{state: st, configPath: filepath.Join(dir, "lever.yaml"), port: port, cfgHash: func() string { return "hash-test" }})
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
		version: "v-test", cfgHash: func() string { return "hash-test" }, selfExe: selfExe}
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

// TestRemoteAwaitStampWaitsForTheProxy: the proxy binds its port before it
// writes remote.pid and its stamp, so apply can see it listening first; it
// waits for the new proxy's own stamp of this config and writes none itself.
func TestRemoteAwaitStampWaitsForTheProxy(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const pid = 4242
	rc := &remoteController{state: st, version: "v-test", cfgHash: func() string { return "hash-test" },
		startTimeout: 2 * time.Second, startInterval: 10 * time.Millisecond}
	if err := os.WriteFile(st.RemotePID(), []byte("4241\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		if err := os.WriteFile(st.RemotePID(), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
			wrote <- err
			return
		}
		time.Sleep(50 * time.Millisecond)
		wrote <- st.WriteRemoteStamp("v-test", "hash-test")
	}()
	if err := rc.awaitStamp(context.Background(), pid, "hash-test"); err != nil {
		t.Fatalf("awaitStamp: %v", err)
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
}

// TestRemoteAwaitStampTimesOut: a proxy that never writes its pid file, or
// stamps another config, leaves no stamp of this one, and the error says why
// (apply prints it as a warning).
func TestRemoteAwaitStampTimesOut(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rc := &remoteController{state: st, version: "v-test", cfgHash: func() string { return "hash-test" },
		startTimeout: 50 * time.Millisecond, startInterval: 5 * time.Millisecond}
	testutil.WantErrContaining(t, rc.awaitStamp(context.Background(), 4242, "hash-test"), "remote.pid")
	if _, serr := os.Stat(st.RemoteStamp()); serr == nil {
		t.Fatal("apply wrote a stamp for a proxy that never wrote its pid file")
	}
	if err := os.WriteFile(st.RemotePID(), []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteRemoteStamp("v-test", "hash-edited"); err != nil {
		t.Fatal(err)
	}
	testutil.WantErrContaining(t, rc.awaitStamp(context.Background(), 4242, "hash-test"), "has not stamped")
	if !st.RemoteStampMatches("v-test", "hash-edited") {
		t.Fatal("apply overwrote the proxy's own stamp")
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

func outsideTree() bool { return false }

// TestRemoteControllerStartReplacesAProxyInsideTheTree: with the state
// directory inside the tree an agent can write the stamp and remote.pid, so a
// stamp that matches vouches for nothing and the proxy is replaced.
func TestRemoteControllerStartReplacesAProxyInsideTheTree(t *testing.T) {
	for name, inTree := range map[string]func() bool{"inside": func() bool { return true }, "unknown": nil} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			st := state.ForConfig(dir)
			if err := os.MkdirAll(st.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
			proxy, proxyDone := testutil.StartServeProcess(t, state.RemoteDaemon, filepath.Join(dir, "lever.yaml"))
			if err := os.WriteFile(st.RemotePID(), []byte(strconv.Itoa(proxy.Process.Pid)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			if err := st.WriteRemoteStamp("v-test", "hash-test"); err != nil {
				t.Fatal(err)
			}
			rc := &remoteController{state: st, configPath: "/x/lever.yaml", port: ln.Addr().(*net.TCPAddr).Port,
				selfExe: "/no/such/lever-binary", version: "v-test", cfgHash: func() string { return "hash-test" }, stateInTree: inTree}
			if err := rc.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "/no/such/lever-binary") {
				t.Fatalf("Start = %v, want the respawn's failure", err)
			}
			select {
			case <-proxyDone:
			case <-time.After(5 * time.Second):
				t.Fatal("a proxy whose stamp an agent could have written was kept")
			}
		})
	}
}

// TestConvergeNotice: up says what changed since the last bring-up that ran
// the whole plan, and nothing when nothing did or nothing was recorded. With
// the state directory inside the tree it neither reads nor writes a record.
func TestConvergeNotice(t *testing.T) {
	root := t.TempDir()
	st := state.State{Dir: filepath.Join(root, "state")}
	app := &config.App{Name: "mgr", Tree: filepath.Join(root, "tree"), CPUs: 2}
	if got := convergeNotice(app, st); got != "" {
		t.Fatalf("nothing recorded: %q", got)
	}
	recordConverge(app, st, "apply", func(string, ...any) { t.Error("warned") })
	if got := convergeNotice(app, st); got != "" {
		t.Fatalf("nothing changed: %q", got)
	}
	edited := *app
	edited.CPUs = 4
	got := convergeNotice(&edited, st)
	if !strings.Contains(got, "the config changed") || !strings.Contains(got, "`lever apply`") || !strings.Contains(got, "this up applies it") {
		t.Fatalf("config edit: %q", got)
	}
	if err := st.RecordConverge("up", convergeDigest(app), "v-older", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := convergeNotice(app, st); !strings.Contains(got, "the lever binary changed") || strings.Contains(got, "config") {
		t.Fatalf("new binary: %q", got)
	}

	inTree := state.State{Dir: filepath.Join(root, "in", "state")}
	in := &config.App{Name: "mgr", Tree: filepath.Join(root, "in"), CPUs: 2}
	recordConverge(in, inTree, "up", func(string, ...any) { t.Error("warned") })
	if _, err := os.Stat(inTree.LifecycleFile()); !os.IsNotExist(err) {
		t.Fatalf("a record was written inside the tree: %v", err)
	}
	if err := os.MkdirAll(inTree.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := inTree.RecordConverge("up", "forged", "forged", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := convergeNotice(in, inTree); got != "" {
		t.Fatalf("a record inside the tree was read: %q", got)
	}
}
