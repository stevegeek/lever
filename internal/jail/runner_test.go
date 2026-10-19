package jail

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/proc"
)

// orbPrefix mirrors the OrbStack backend's JailPrefix; the jail package
// itself is backend-agnostic.
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
		"A=1", "true"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv =\n %v\nwant\n %v", got, want)
	}
}

func TestForceHostNetworkEscapeHatch(t *testing.T) {
	// Only a truthy value re-emits SCION_FORCE_HOST_NETWORK; =0, =false and
	// unparseable values stay off, so a surprising value on this security
	// knob never re-opens host networking.
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

type capRecorder struct {
	*proc.FakeRunner
	caps []int
}

func (r *capRecorder) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	n, _ := proc.OutputLimit(ctx)
	r.caps = append(r.caps, n)
	return r.FakeRunner.Run(ctx, env, name, args...)
}

func (r *capRecorder) RunStdin(ctx context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	n, _ := proc.OutputLimit(ctx)
	r.caps = append(r.caps, n)
	return r.FakeRunner.RunStdin(ctx, stdin, env, name, args...)
}

// Config.OutputLimit caps a command whose context has no cap, on every run
// path and with the hub token on stdin too; a cap the caller set wins
// whether it is smaller or larger, and 0 leaves commands uncapped.
func TestJailRunnerOutputLimit(t *testing.T) {
	host := &capRecorder{FakeRunner: proc.NewFakeRunner()}
	host.Script("orb", proc.Result{Stdout: "ok"})
	jr := New(Config{Host: host, Prefix: orbPrefix("m", "u"), UID: "501", OutputLimit: 1000})
	ctx := context.Background()
	tok := map[string]string{hubTokenEnv: "scion_pat_abc"}
	_, _ = jr.Run(ctx, nil, "scion", "list")
	_, _ = jr.RunIn(ctx, "/w", tok, "scion", "list")
	_, _ = jr.RunStdin(ctx, strings.NewReader("x"), nil, "sh")
	_, _ = jr.RunStdin(ctx, nil, tok, "sh")
	_, _ = jr.Run(proc.WithOutputLimit(ctx, 10), nil, "date")
	_, _ = jr.Run(proc.WithOutputLimit(ctx, 5000), nil, "capture")
	_, _ = New(Config{Host: host, Prefix: orbPrefix("m", "u"), UID: "501"}).Run(ctx, nil, "scion", "list")
	if want := []int{1000, 1000, 1000, 1000, 10, 5000, 0}; !slices.Equal(host.caps, want) {
		t.Fatalf("caps = %v, want %v", host.caps, want)
	}
}

// A jail command that floods its output is killed and fails with
// proc.ErrOutputLimit; one under the cap returns its output.
func TestJailRunnerOutputLimitStopsAFlood(t *testing.T) {
	if _, err := exec.LookPath("yes"); err != nil {
		t.Skip("no yes(1)")
	}
	jr := New(Config{Host: proc.RealRunner{}, Prefix: []string{"env"}, UID: "501", OutputLimit: 64 << 10})
	start := time.Now()
	res, err := jr.Run(context.Background(), nil, "yes")
	if !errors.Is(err, proc.ErrOutputLimit) || len(res.Stdout) > 64<<10 {
		t.Fatalf("flood: err %v, %d bytes", err, len(res.Stdout))
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("flood took %v", d)
	}
	res, err = jr.Run(context.Background(), nil, "echo", "fine")
	if err != nil || res.Stdout != "fine\n" {
		t.Fatalf("small output: %q %v", res.Stdout, err)
	}
}
