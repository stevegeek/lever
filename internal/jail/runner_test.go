package jail

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/exec"
)

func TestJailRunnerWrapsWithOrbEnv(t *testing.T) {
	host := exec.NewFakeRunner()
	host.Script("orb", exec.Result{Stdout: "ok"})
	jr := New(host, OrbPrefix("lever-jail", "leveruser"), "501")
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
	jr := New(host, OrbPrefix("lever-jail", "leveruser"), "501")
	_, _ = jr.RunIn(context.Background(), "/lever/workers/worker", nil, "scion", "init", "--non-interactive")
	got := strings.Join(host.Calls[0].Args, " ")
	if !strings.Contains(got, "env -C /lever/workers/worker") {
		t.Fatalf("expected env -C <dir>; got %q", got)
	}
	if !strings.Contains(got, "scion init --non-interactive") {
		t.Fatalf("missing command; got %q", got)
	}
}

func TestPrefixIsBackendShaped(t *testing.T) {
	// A lima-shaped prefix produces limactl argv with the same env handling.
	t.Setenv("LEVER_FORCE_HOST_NETWORK", "") // pin the default regardless of ambient env
	host := exec.NewFakeRunner()
	host.Script("limactl", exec.Result{})
	jr := New(host, []string{"limactl", "shell", "lever-x"}, "501")
	_, err := jr.Run(context.Background(), map[string]string{"A": "1"}, "true")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := append([]string{host.Calls[0].Name}, host.Calls[0].Args...)
	// Default (LEVER_FORCE_HOST_NETWORK unset): agents run in their own pasta
	// netns, so SCION_FORCE_HOST_NETWORK is NOT emitted.
	want := []string{"limactl", "shell", "lever-x", "env",
		"XDG_RUNTIME_DIR=/run/user/501", "PATH=/usr/local/bin:/usr/bin:/bin",
		"SCION_HUB_ENABLED=true", "A=1", "true"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv =\n %v\nwant\n %v", got, want)
	}
}

func TestForceHostNetworkEscapeHatch(t *testing.T) {
	// The escape hatch parses a bool: truthy re-emits scion's
	// SCION_FORCE_HOST_NETWORK (fall back to --network=host); everything else —
	// crucially =0/=false and unparseable values — stays OFF (own netns), so a
	// surprising value on this security knob never silently re-opens the gap.
	cases := []struct {
		val  string
		want bool
	}{
		{"1", true}, {"true", true}, {"t", true},
		{"0", false}, {"false", false}, {"", false}, {"yes", false},
	}
	for _, c := range cases {
		t.Run("val="+c.val, func(t *testing.T) {
			t.Setenv("LEVER_FORCE_HOST_NETWORK", c.val)
			host := exec.NewFakeRunner()
			host.Script("orb", exec.Result{})
			jr := New(host, OrbPrefix("lever-x", "leveruser"), "501")
			if _, err := jr.Run(context.Background(), nil, "true"); err != nil {
				t.Fatalf("run: %v", err)
			}
			got := append([]string{host.Calls[0].Name}, host.Calls[0].Args...)
			if has := slices.Contains(got, "SCION_FORCE_HOST_NETWORK=1"); has != c.want {
				t.Fatalf("val %q: host-net emitted=%v, want %v; argv=%v", c.val, has, c.want, got)
			}
		})
	}
}
