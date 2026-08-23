package host

import (
	"fmt"
	"testing"

	"github.com/stevegeek/lever/internal/cli/clitest"
	"github.com/stevegeek/lever/internal/config"
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
	clitest.WantErrContaining(t, err, "nope", "assistant", "scratch", "worker")
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
