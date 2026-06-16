package jail

import (
	"context"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/exec"
)

func TestJailRunnerWrapsWithOrbEnv(t *testing.T) {
	host := exec.NewFakeRunner()
	host.Script("orb", exec.Result{Stdout: "ok"})
	jr := New(host, "lever-jail", "leveruser", "501")
	_, err := jr.Run(context.Background(), map[string]string{"SCION_HUB_ENDPOINT": "http://127.0.0.1:8080"}, "scion", "list", "--format", "json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if host.Calls[0].Name != "orb" {
		t.Fatalf("must invoke orb, got %q", host.Calls[0].Name)
	}
	got := strings.Join(host.Calls[0].Args, " ")
	for _, want := range []string{"-m lever-jail", "-u leveruser", "env", "XDG_RUNTIME_DIR=/run/user/501", "SCION_HUB_ENDPOINT=http://127.0.0.1:8080", "scion list --format json"} {
		if !strings.Contains(got, want) {
			t.Fatalf("orb argv %q missing %q", got, want)
		}
	}
}

func TestJailRunnerRunInUsesEnvChdir(t *testing.T) {
	host := exec.NewFakeRunner()
	host.Script("orb", exec.Result{})
	jr := New(host, "lever-jail", "leveruser", "501")
	_, _ = jr.RunIn(context.Background(), "/lever/groves/worker", nil, "scion", "init", "--non-interactive")
	got := strings.Join(host.Calls[0].Args, " ")
	if !strings.Contains(got, "env -C /lever/groves/worker") {
		t.Fatalf("expected env -C <dir>; got %q", got)
	}
	if !strings.Contains(got, "scion init --non-interactive") {
		t.Fatalf("missing command; got %q", got)
	}
}
