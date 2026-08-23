package jail

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

// orbPrefix is a test-only helper mirroring the OrbStack backend's JailPrefix
// (["orb","-m",machine,"-u",user]); the jail package itself is backend-agnostic
// and no longer exports it.
func orbPrefix(machine, user string) []string {
	return []string{"orb", "-m", machine, "-u", user}
}

func TestJailRunnerWrapsWithOrbEnv(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{Stdout: "ok"})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-jail", "leveruser"), UID: "501"})
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
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-jail", "leveruser"), UID: "501"})
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
	host := proc.NewFakeRunner()
	host.Script("limactl", proc.Result{})
	jr := New(Config{Host: host, Prefix: []string{"limactl", "shell", "lever-x"}, UID: "501"})
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
			t.Setenv(ForceHostNetworkEnv, c.val)
			if got := ForceHostNetworkFromEnv(); got != c.want {
				t.Fatalf("ForceHostNetworkFromEnv() with %q = %v, want %v", c.val, got, c.want)
			}
		})
	}
	// The transport itself is pure: it emits the knob iff Config says so.
	for _, force := range []bool{false, true} {
		host := proc.NewFakeRunner()
		host.Script("orb", proc.Result{})
		jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "leveruser"), UID: "501", ForceHostNetwork: force})
		if _, err := jr.Run(context.Background(), nil, "true"); err != nil {
			t.Fatalf("run: %v", err)
		}
		got := append([]string{host.Calls[0].Name}, host.Calls[0].Args...)
		if has := slices.Contains(got, "SCION_FORCE_HOST_NETWORK=1"); has != force {
			t.Fatalf("ForceHostNetwork=%v: host-net emitted=%v; argv=%v", force, has, got)
		}
		if has := slices.Contains(jr.AttachArgv([]string{"true"}), "SCION_FORCE_HOST_NETWORK=1"); has != force {
			t.Fatalf("ForceHostNetwork=%v: AttachArgv host-net emitted=%v", force, has)
		}
	}
}
