package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if closed, warn := closedApp.ClosedInternetEgress(); !closed || warn != "" {
		t.Fatalf("egress: closed must resolve closed egress: closed=%v warn=%q", closed, warn)
	}
	openApp := &config.App{Broker: config.Broker{LLMAuth: config.LLMAuthAPIKey, JailPort: 8443}}
	if closed, _ := openApp.ClosedInternetEgress(); closed {
		t.Fatal("api-key WITHOUT egress: closed must leave egress open (decoupled)")
	}
}

// TestApplyOpenEgressForSubscription verifies that a subscription instance does
// not set the closed posture (and emits no warning since it's a pure subscription).
func TestApplyOpenEgressForSubscription(t *testing.T) {
	app := &config.App{Broker: config.Broker{LLMAuth: config.LLMAuthSubscription, JailPort: 8443}}
	closed, warn := app.ClosedInternetEgress()
	if closed {
		t.Fatalf("subscription instance must not resolve closed egress")
	}
	if warn != "" {
		t.Fatalf("pure subscription must not produce warning; got %q", warn)
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
	f.Script("scion hub token create", leverexec.Result{Stdout: "pat-wired-abc\n"})
	f.Script("scion server stop", leverexec.Result{})
	f.Script("sh", leverexec.Result{})
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
	f.Script("scion hub token create", leverexec.Result{Stdout: "pat-mint-xyz\n"})
	f.Script("scion server stop", leverexec.Result{})
	f.Script("sh", leverexec.Result{})

	if err := ensureControllerPAT(context.Background(), f, state, tree, jailMount); err != nil {
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
	iRm := callIndex(f.Calls, func(c leverexec.Call) bool { return c.Name == "sh" })
	if iStart < 0 || iInit < 0 || iLink < 0 || iToken < 0 || iStop < 0 || iRm < 0 {
		t.Fatalf("missing expected call(s); calls=%+v", f.Calls)
	}
	if !(iStart < iInit && iInit < iLink && iLink < iToken && iToken < iStop) {
		t.Fatalf("calls out of order: start=%d init=%d link=%d token=%d stop=%d", iStart, iInit, iLink, iToken, iStop)
	}

	// Fixed throwaway port, distinct from the real hub's 8080; dev-auth ON.
	startArgs := strings.Join(f.Calls[iStart].Args, " ")
	if !strings.Contains(startArgs, "--port 48080") || !strings.Contains(startArgs, "--dev-auth=true") {
		t.Fatalf("throwaway server start args = %q, want --port 48080 --dev-auth=true", startArgs)
	}

	// init/hub-link run inside the jail project dir (the tree root).
	if f.Calls[iInit].Dir != jailMount {
		t.Fatalf("init dir = %q, want %q", f.Calls[iInit].Dir, jailMount)
	}
	if f.Calls[iLink].Dir != jailMount {
		t.Fatalf("hub link dir = %q, want %q", f.Calls[iLink].Dir, jailMount)
	}

	// Exact scopes string — no agent:message (every interactive verb,
	// message included, gates on agent:attach; see the P3 plan).
	tokenArgs := strings.Join(f.Calls[iToken].Args, " ")
	if !strings.Contains(tokenArgs, "--scopes agent:manage,agent:attach,project:read") {
		t.Fatalf("hub token create args = %q, want --scopes agent:manage,agent:attach,project:read", tokenArgs)
	}

	// Best-effort dev-token removal ran through the SAME jail runner as an
	// `sh -c` guard (mirrors Deps.RemoveJailFile's script — see
	// removeJailFileScript).
	rm := f.Calls[iRm]
	if len(rm.Args) != 4 || rm.Args[0] != "-c" || rm.Args[2] != "_" || rm.Args[3] != "/home/scion/.scion/dev-token" {
		t.Fatalf("dev-token removal args = %+v, want [-c <script> _ /home/scion/.scion/dev-token]", rm.Args)
	}

	callsAfterFirst := len(f.Calls)

	// Second call: PAT already persisted → no-op. In particular, no second
	// throwaway server start (the agent-free mint window opens at most once).
	if err := ensureControllerPAT(context.Background(), f, state, tree, jailMount); err != nil {
		t.Fatalf("second ensureControllerPAT: %v", err)
	}
	if len(f.Calls) != callsAfterFirst {
		t.Fatalf("second call made %d new runner call(s), want 0 (must be a no-op): %+v",
			len(f.Calls)-callsAfterFirst, f.Calls[callsAfterFirst:])
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
	f.Script("scion hub token create", leverexec.Result{Stdout: "pat-e2e-round-trip\n"})
	f.Script("scion server stop", leverexec.Result{})
	f.Script("sh", leverexec.Result{})
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
	if err := deps.Scion.ServerStart(ctx, scion.ServerOpts{Port: 8080, DevAuth: false}); err != nil {
		t.Fatalf("scion-server step: %v", err)
	}

	// bootstrap-token precedes scion-server: the throwaway (48080, dev-auth
	// ON) server start must land BEFORE the real hub's (8080, dev-auth OFF).
	iThrowaway := callIndex(f.Calls, func(c leverexec.Call) bool {
		return callHasPrefix(c, "scion server start --port 48080")
	})
	iReal := callIndex(f.Calls, func(c leverexec.Call) bool {
		return callHasPrefix(c, "scion server start --port 8080")
	})
	if iThrowaway < 0 || iReal < 0 {
		t.Fatalf("missing server-start call(s); calls=%+v", f.Calls)
	}
	if !(iThrowaway < iReal) {
		t.Fatalf("throwaway server start (call %d) must precede the real hub server start (call %d)", iThrowaway, iReal)
	}

	// scion-server locks the real hub: port 8080, dev-auth off.
	realArgs := strings.Join(f.Calls[iReal].Args, " ")
	if !strings.Contains(realArgs, "--port 8080") || !strings.Contains(realArgs, "--dev-auth=false") {
		t.Fatalf("real hub server start args = %q, want --port 8080 --dev-auth=false", realArgs)
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
	if err := deps2.Scion.ServerStart(ctx, scion.ServerOpts{Port: 8080, DevAuth: false}); err != nil {
		t.Fatalf("scion-server step (2nd apply): %v", err)
	}
	// The 2nd apply's real hub server-start is the LAST such call (ServerStart
	// itself appends a trailing waitHubReady "list" call right after it, so
	// it is not simply the last entry in f.Calls).
	iReal2 := -1
	for idx, c := range f.Calls {
		if callHasPrefix(c, "scion server start --port 8080") {
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
