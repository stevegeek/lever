package jail

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

func TestAgentProbeLogTail(t *testing.T) {
	var calls []string
	p := AgentProbe{R: codeRunner{res: proc.Result{Stdout: "out line", Stderr: "err line"}, calls: &calls}}
	got, err := p.LogTail(context.Background(), "lever--assistant")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "out line") || !strings.Contains(got, "err line") {
		t.Fatalf("tail %q must hold stdout and stderr", got)
	}
	if calls[0] != "podman logs --tail 200 lever--assistant" {
		t.Fatalf("argv %q", calls[0])
	}
}

func TestAgentProbeLogTailErrors(t *testing.T) {
	var calls []string
	_, err := AgentProbe{R: codeRunner{res: proc.Result{Code: 125, Stderr: "Error: no such container x"}, calls: &calls}}.LogTail(context.Background(), "x")
	if !errors.Is(err, ErrNoContainer) {
		t.Fatalf("err = %v, want ErrNoContainer", err)
	}
	_, err = AgentProbe{R: codeRunner{res: proc.Result{Code: 1, Stderr: "SECRET"}, calls: &calls}}.LogTail(context.Background(), "x")
	var exit *ProbeExitError
	if !errors.As(err, &exit) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v, want a ProbeExitError without the container's words", err)
	}
	if _, err := (AgentProbe{R: codeRunner{calls: &calls}}).LogTail(context.Background(), "--all"); err == nil {
		t.Fatal("a flag-shaped ref must be refused")
	}
}
