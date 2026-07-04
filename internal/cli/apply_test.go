package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/config"
	leverexec "github.com/lever-to/lever/internal/exec"
)

// writeTmpConfig writes a minimal app.yaml with a real tree directory structure
// and returns the config file path. Mirrors config_test.go's writeTmp.
func writeTmpConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "groves", "worker"), 0o755); err != nil {
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
groves:
  - name: worker
    dir: groves/worker
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
// internal/apply/run.go's register-manager/register-grove case for the
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
// so the register-manager/register-grove step in internal/apply/run.go can
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
	if err := deps.RemoveScionProjectConfigs(context.Background(), "/lever/groves/worker"); err != nil {
		t.Fatalf("RemoveScionProjectConfigs: %v", err)
	}
	if len(sb.removeScionCalls) != 1 || sb.removeScionCalls[0] != "/lever/groves/worker" {
		t.Fatalf("backend.RemoveScionProjectConfigs calls = %+v, want exactly one call with \"/lever/groves/worker\"", sb.removeScionCalls)
	}
}

// TestBuildApplyDepsWiresScionProjectRegistered verifies buildApplyDeps wires
// Deps.ScionProjectRegistered straight through to the backend method, so the
// register-manager/register-grove step (internal/apply/run.go) can observe
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
	ok, err := deps.ScionProjectRegistered(context.Background(), "/lever/groves/worker")
	if err != nil {
		t.Fatalf("ScionProjectRegistered: %v", err)
	}
	if !ok {
		t.Fatal("expected the stubbed true result to pass through")
	}
	if len(sb.registeredCalls) != 1 || sb.registeredCalls[0] != "/lever/groves/worker" {
		t.Fatalf("backend.ScionProjectRegistered calls = %+v, want exactly one call with \"/lever/groves/worker\"", sb.registeredCalls)
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
