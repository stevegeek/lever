package cli

import (
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/config"
)

func attachApp() *config.App {
	return &config.App{
		Name: "assistant",
		Groves: []config.Grove{
			{Name: "scratch", Dir: "groves/scratch"},
			{Name: "worker", Dir: "groves/worker"},
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

func TestAttachTargetGrove(t *testing.T) {
	slug, project, err := attachTarget(attachApp(), "/lever", "scratch")
	if err != nil {
		t.Fatalf("attachTarget: %v", err)
	}
	if slug != "scratch" || project != "/lever/groves/scratch" {
		t.Fatalf("got (%q, %q), want (scratch, /lever/groves/scratch)", slug, project)
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
