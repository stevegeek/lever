package common

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
)

func newTestBase(opts Options) Base {
	return NewBase(Config{
		Runner:  proc.NewFakeRunner(),
		Machine: "m",
		Hooks: Hooks{
			JailPrefix: func(machine, _ string) []string { return []string{"enter", machine} },
		},
		Options: opts,
	})
}

// TestForceHostNetworkComesFromOptionsNotEnv pins that Base takes the
// host-network escape hatch from Options only: the environment is read once,
// by the registry, at construction.
func TestForceHostNetworkComesFromOptionsNotEnv(t *testing.T) {
	const marker = "SCION_FORCE_HOST_NETWORK=1"
	t.Setenv(jail.ForceHostNetworkEnv, "1")
	b := newTestBase(Options{})
	if slices.Contains(b.AttachArgv([]string{"true"}), marker) {
		t.Fatalf("Base read %s from the environment; argv = %v", jail.ForceHostNetworkEnv, b.AttachArgv([]string{"true"}))
	}

	t.Setenv(jail.ForceHostNetworkEnv, "0")
	b = newTestBase(Options{ForceHostNetwork: true})
	if !slices.Contains(b.AttachArgv([]string{"true"}), marker) {
		t.Fatalf("Options.ForceHostNetwork not applied; argv = %v", b.AttachArgv([]string{"true"}))
	}
}

// The run user goes into root scripts, file names and transport argv, so a
// guest reporting anything but a plain name and uid stops the bring-up and
// leaves nothing resolved.
func TestReadRunUserRefusesWhatIsNotAPlainUser(t *testing.T) {
	for _, c := range []struct{ user, uid string }{
		{"alice'; reboot; '", "1000"},
		{"../../etc", "1000"},
		{"alice", "1000; id"},
		{"root", "0"},
		{"\x1b]0;x\x07", "1000"},
	} {
		f := proc.NewFakeRunner()
		f.Script("u whoami", proc.Result{Stdout: c.user + "\n"})
		f.Script("u id -u", proc.Result{Stdout: c.uid + "\n"})
		b := NewBase(Config{Runner: f, Machine: "m", Hooks: Hooks{
			Guest: func(r proc.Runner, m string) guest.Guest {
				return guest.Guest{Host: r, UserPrefix: []string{"u"}, RootPrefix: []string{"r"}, Machine: m}
			},
		}})
		err := b.ReadRunUser(context.Background())
		if err == nil || !strings.Contains(err.Error(), "resolve run user") {
			t.Errorf("%q %q: err = %v", c.user, c.uid, err)
		}
		if b.RunUser() != "" || b.RunUID() != DefaultRunUID {
			t.Errorf("%q %q: resolved %q %q", c.user, c.uid, b.RunUser(), b.RunUID())
		}
	}
	f := proc.NewFakeRunner()
	f.Script("u whoami", proc.Result{Stdout: "first.last\n"})
	f.Script("u id -u", proc.Result{Stdout: "501\n"})
	b := NewBase(Config{Runner: f, Machine: "m", Hooks: Hooks{
		Guest: func(r proc.Runner, m string) guest.Guest { return guest.Guest{Host: r, UserPrefix: []string{"u"}} },
	}})
	if err := b.ReadRunUser(context.Background()); err != nil || b.RunUser() != "first.last" || b.RunUID() != "501" {
		t.Fatalf("a plain user: %q %q %v", b.RunUser(), b.RunUID(), err)
	}
}

type limitRecorder struct {
	*proc.FakeRunner
	limits []int
}

func (r *limitRecorder) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	n, _ := proc.OutputLimit(ctx)
	r.limits = append(r.limits, n)
	return r.FakeRunner.Run(ctx, env, name, args...)
}

// The jail image commands of the bring-up run under the guest output cap.
func TestJailImageCommandsRunUnderTheGuestOutputLimit(t *testing.T) {
	r := &limitRecorder{FakeRunner: proc.NewFakeRunner()}
	r.Script("", proc.Result{Stdout: "sha256:abc\n"})
	b := NewBase(Config{Runner: r, Machine: "m", Hooks: Hooks{
		JailPrefix: func(machine, _ string) []string { return []string{"enter", machine} },
	}})
	ctx := context.Background()
	_ = b.PruneJailImages(ctx)
	_ = b.WarmJailImage(ctx, "img:1")
	_ = b.ImageLoaded(ctx, "img:1")
	if len(r.limits) < 4 {
		t.Fatalf("%d calls", len(r.limits))
	}
	for i, n := range r.limits {
		if n != guest.OutputLimit {
			t.Errorf("call %d (%s) ran with cap %d", i, r.Calls[i].Argv(), n)
		}
	}
}

// A jail command with no cap of its own (the scion CLI, the probes) runs
// under the guest output cap; one that set its own keeps it.
func TestJailRunnerRunsUnderTheGuestOutputLimit(t *testing.T) {
	r := &limitRecorder{FakeRunner: proc.NewFakeRunner()}
	r.Script("", proc.Result{Stdout: "[]\n"})
	b := NewBase(Config{Runner: r, Machine: "m", Hooks: Hooks{
		JailPrefix: func(machine, _ string) []string { return []string{"enter", machine} },
	}})
	ctx := context.Background()
	_, _ = b.JailRunner().Run(ctx, nil, "scion", "list", "--format", "json")
	_, _ = b.JailRunner().Run(proc.WithOutputLimit(ctx, 512<<20), nil, "capture")
	if !slices.Equal(r.limits, []int{guest.OutputLimit, 512 << 20}) {
		t.Fatalf("caps = %v", r.limits)
	}
}
