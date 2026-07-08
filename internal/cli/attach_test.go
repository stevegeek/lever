package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
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
	if slug != "scratch" || project != "/lever/workers/scratch" {
		t.Fatalf("got (%q, %q), want (scratch, /lever/workers/scratch)", slug, project)
	}
}

func TestAttachTargetUnknownListsValidNames(t *testing.T) {
	_, _, err := attachTarget(attachApp(), "/lever", "nope")
	if err == nil {
		t.Fatal("want error for unknown name")
	}
	for _, want := range []string{"nope", "assistant", "scratch", "worker"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestAttachIsPassiveWhenJailNotUp is the regression test for the reviewed
// finding: `lever attach` against a down jail must fail fast with a
// `lever up` hint, never provision the machine (no buildApplyDeps/EnsureUp).
func TestAttachIsPassiveWhenJailNotUp(t *testing.T) {
	dir := instanceDir(t, "demo")
	t.Chdir(dir)

	sb := &stubBackend{resolveRunUserErr: fmt.Errorf("machine %q does not exist", "lever-demo")}
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return sb, nil })
	root.SetArgs([]string{"attach"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)

	err := root.Execute()
	if err == nil {
		t.Fatal("expected attach to fail when the jail is not up")
	}
	if !strings.Contains(err.Error(), "lever up") {
		t.Fatalf("error should tell the operator to run `lever up`; got: %v", err)
	}
	if sb.up {
		t.Fatal("attach must never call EnsureUp — it must not provision the jail")
	}
}
