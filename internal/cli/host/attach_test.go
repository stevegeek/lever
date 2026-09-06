package host

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/cli/clitest"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/testutil"
)

func attachApp() *config.App {
	return &config.App{
		Name: "assistant",
		Workers: []config.Worker{
			{Name: "scratch", Dir: "workers/scratch"},
			{Name: "worker", Dir: "workers/worker"},
		},
	}
}

// wantAttachTarget resolves `to` against attachApp and requires slug in the
// instance project /lever.
func wantAttachTarget(t *testing.T, to, wantSlug string) {
	t.Helper()
	slug, project, err := attachTarget(attachApp(), "/lever", to)
	if err != nil {
		t.Fatalf("attachTarget: %v", err)
	}
	if slug != wantSlug || project != "/lever" {
		t.Fatalf("got (%q, %q), want (%s, /lever)", slug, project, wantSlug)
	}
}

func TestAttachTargetDefaultsToManager(t *testing.T) { wantAttachTarget(t, "", "assistant") }

func TestAttachTargetManagerByName(t *testing.T) { wantAttachTarget(t, "assistant", "assistant") }

// Single-project model: the worker's agent record lives in the instance
// project (the jail mount root), NOT a per-worker /lever/workers/<name>.
func TestAttachTargetWorker(t *testing.T) { wantAttachTarget(t, "scratch", "scratch") }

func TestAttachTargetUnknownListsValidNames(t *testing.T) {
	_, _, err := attachTarget(attachApp(), "/lever", "nope")
	if err == nil {
		t.Fatal("want error for unknown name")
	}
	testutil.WantErrContaining(t, err, "nope", "assistant", "scratch", "worker")
}

// TestAttachNamePositionalIsNotAConfigPath pins that `attach <name>`'s positional
// is the agent NAME (fed to attachTarget), never the config path: config is always
// discovered from the CWD (the explicit-empty variant). A regression that mistook
// the NAME for a config path would fail at config.Load("scratch") with a
// file-not-found error, never reaching the jail-not-up hint.
func TestAttachNamePositionalIsNotAConfigPath(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	sb := &stubBackend{resolveRunUserErr: fmt.Errorf("machine %q does not exist", "lever-demo")}
	root := stubRoot(sb)
	_, err := clitest.Exec(t, root, "attach", "scratch")
	// Reaching the jail-not-up hint proves lever.yaml loaded from the CWD and the
	// positional "scratch" was NOT treated as a config path.
	wantJailNotUp(t, err)
}

// TestAttachIsPassiveWhenJailNotUp is the regression test for the reviewed
// finding: `lever attach` against a down jail must fail fast with a
// `lever up` hint, never provision the machine (no buildApplyDeps/EnsureUp).
func TestAttachIsPassiveWhenJailNotUp(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	sb := &stubBackend{resolveRunUserErr: fmt.Errorf("machine %q does not exist", "lever-demo")}
	root := stubRoot(sb)
	_, err := clitest.Exec(t, root, "attach")
	wantJailNotUp(t, err)
	if sb.up {
		t.Fatal("attach must never call EnsureUp — it must not provision the jail")
	}
}

// TestAttachArgvKeepsHubTokenOffHostArgv: `lever attach` execs a host process
// (`orb …`) whose argv every local user can read with `ps` for the whole
// session. With a controller PAT in state, the token is staged in the guest
// through the jail runner (on stdin) and the exec'd argv reads it there —
// the PAT itself appears in no host argv at all.
func TestAttachArgvKeepsHubTokenOffHostArgv(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{})
	jr := jail.New(jail.Config{Host: host, Prefix: []string{"orb", "-m", "lever-demo", "-u", "leveruser"}, UID: "501"})
	sb := &stubBackend{runner: jr}
	sc := scion.New(jr, scion.Options{HubEndpoint: scion.DefaultHubEndpoint, HubTokenSource: func() string { return "pat-attach-secret" }})

	argv, err := attachArgv(context.Background(), sb, sc, "assistant", "/lever")
	if err != nil {
		t.Fatalf("attachArgv: %v", err)
	}
	if joined := strings.Join(argv, " "); strings.Contains(joined, "pat-attach-secret") {
		t.Fatalf("token in the exec argv: %q", joined)
	} else if !strings.Contains(joined, "SCION_HUB_ENDPOINT="+scion.DefaultHubEndpoint) || !strings.HasSuffix(joined, "scion attach assistant -g /lever") {
		t.Fatalf("exec argv must still pin the endpoint and end with the attach command: %q", joined)
	}
	if len(host.Calls) != 1 {
		t.Fatalf("want one staging call through the jail, got %d", len(host.Calls))
	}
	stage := host.Calls[0]
	if joined := strings.Join(append([]string{stage.Name}, stage.Args...), " "); strings.Contains(joined, "pat-attach-secret") {
		t.Fatalf("token in the staging host argv: %q", joined)
	}
	if stage.Stdin != "pat-attach-secret" {
		t.Fatalf("staging stdin = %q, want the token", stage.Stdin)
	}
}

// Without a token (subscription mode) nothing is staged and the argv is the
// plain backend-wrapped attach.
func TestAttachArgvWithoutTokenStagesNothing(t *testing.T) {
	host := proc.NewFakeRunner()
	sb := &stubBackend{runner: host}
	sc := scion.New(host, scion.Options{HubEndpoint: scion.DefaultHubEndpoint})
	argv, err := attachArgv(context.Background(), sb, sc, "assistant", "/lever")
	if err != nil {
		t.Fatalf("attachArgv: %v", err)
	}
	want := []string{"stub-attach", "env", "SCION_HUB_ENDPOINT=" + scion.DefaultHubEndpoint, "scion", "attach", "assistant", "-g", "/lever"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
	if len(host.Calls) != 0 {
		t.Fatalf("nothing may run without a token; got %d call(s)", len(host.Calls))
	}
}
