package host

import (
	"fmt"
	"github.com/stevegeek/lever/internal/cli/clitest"
	"testing"

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

func TestAttachTargetDefaultsToManager(t *testing.T) {
	slug, project, err := attachTarget(attachApp(), "/lever", "")
	if err != nil {
		t.Fatalf("attachTarget: %v", err)
	}
	if slug != "assistant" || project != "/lever" {
		t.Fatalf("got (%q, %q), want (assistant, /lever)", slug, project)
	}
}

func TestAttachTargetManagerByName(t *testing.T) {
	slug, project, err := attachTarget(attachApp(), "/lever", "assistant")
	if err != nil {
		t.Fatalf("attachTarget: %v", err)
	}
	if slug != "assistant" || project != "/lever" {
		t.Fatalf("got (%q, %q), want (assistant, /lever)", slug, project)
	}
}

func TestAttachTargetWorker(t *testing.T) {
	slug, project, err := attachTarget(attachApp(), "/lever", "scratch")
	if err != nil {
		t.Fatalf("attachTarget: %v", err)
	}
	// Single-project model: the worker's agent record lives in the instance
	// project (the jail mount root), NOT a per-worker /lever/workers/<name>.
	if slug != "scratch" || project != "/lever" {
		t.Fatalf("got (%q, %q), want (scratch, /lever)", slug, project)
	}
}

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
