package apply

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/testutil"
)

type fakeGuestBoot struct {
	current, recorded string
	currentErr        error
	records           []string
}

func (g *fakeGuestBoot) Current(context.Context) (string, error) { return g.current, g.currentErr }
func (g *fakeGuestBoot) Recorded() (string, error)               { return g.recorded, nil }
func (g *fakeGuestBoot) Record(id string) error {
	g.records = append(g.records, id)
	g.recorded = id
	return nil
}

const (
	bootA = "11111111-1111-4111-8111-111111111111"
	bootB = "22222222-2222-4222-8222-222222222222"
)

func podmanSays(running bool) func(context.Context, string) (bool, error) {
	return func(context.Context, string) (bool, error) { return running, nil }
}

// After a hard VM restart the hub record still says running over a container
// that never started: apply suspends and resumes it (keeping the
// conversation) instead of refusing, and records the new boot.
func TestStartManagerRecoversAfterGuestRestart(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "created"}
	gb := &fakeGuestBoot{current: bootB, recorded: bootA}
	var logged logSink
	deps := Deps{
		Scion:            scion.New(r, scion.Options{}),
		GuestBoot:        gb,
		ContainerRunning: podmanSays(false),
		Log:              logged.logf,
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.suspendCalls != 1 || r.resumeCalls != 1 || r.resumeForceCalls != 0 {
		t.Errorf("suspend=%d resume=%d force=%d, want 1/1/0", r.suspendCalls, r.resumeCalls, r.resumeForceCalls)
	}
	if r.startCalls != 0 || r.deleteCalls != 0 {
		t.Errorf("start=%d delete=%d: the conversation must be kept", r.startCalls, r.deleteCalls)
	}
	if len(gb.records) != 1 || gb.records[0] != bootB {
		t.Errorf("records = %v, want the new boot", gb.records)
	}
	if !strings.Contains(strings.Join(logged.lines, "\n"), "the jail VM restarted since the manager last ran") {
		t.Errorf("the recovery must be logged:\n%s", strings.Join(logged.lines, "\n"))
	}
}

// Same boot: a harness that died in a live guest is not this case, and the
// gate refuses as before, with nothing suspended.
func TestStartManagerSameBootDoesNotRecover(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "stopped"}
	deps := Deps{
		ManagerLiveRetry: fastRetry(3),
		Scion:            scion.New(r, scion.Options{}),
		GuestBoot:        &fakeGuestBoot{current: bootA, recorded: bootA},
		ContainerRunning: podmanSays(false),
	}
	err := runApply(app, deps)
	testutil.WantErrContaining(t, err, `phase "running"`)
	if r.suspendCalls != 0 || r.resumeCalls != 0 {
		t.Errorf("suspend=%d resume=%d, want 0/0", r.suspendCalls, r.resumeCalls)
	}
}

func TestGuestRestartedUnder(t *testing.T) {
	running := &scion.Agent{Slug: "hello", Phase: scion.PhaseRunning, ContainerStatus: "created"}
	cases := []struct {
		name string
		rec  *scion.Agent
		gb   GuestBoot
		cr   func(context.Context, string) (bool, error)
		want bool
	}{
		{"new boot, podman says stopped", running, &fakeGuestBoot{current: bootB, recorded: bootA}, podmanSays(false), true},
		{"new boot, podman says running", running, &fakeGuestBoot{current: bootB, recorded: bootA}, podmanSays(true), false},
		// The hub reports "created" for a starting container too, so
		// without podman's answer nothing is suspended.
		{"new boot, no podman, hub created", running, &fakeGuestBoot{current: bootB, recorded: bootA}, nil, false},
		{"new boot, podman fails, hub created", running, &fakeGuestBoot{current: bootB, recorded: bootA},
			func(context.Context, string) (bool, error) { return false, errors.New("podman: no answer") }, false},
		{"new boot, podman fails, hub exited", &scion.Agent{Slug: "hello", Phase: scion.PhaseRunning, ContainerStatus: "Exited (137)"}, &fakeGuestBoot{current: bootB, recorded: bootA},
			func(context.Context, string) (bool, error) { return false, errors.New("podman: no answer") }, false},
		{"new boot, podman fails, hub blank", &scion.Agent{Slug: "hello", Phase: scion.PhaseRunning}, &fakeGuestBoot{current: bootB, recorded: bootA},
			func(context.Context, string) (bool, error) { return false, errors.New("no podman") }, false},
		{"new boot, no podman, hub live", &scion.Agent{Slug: "hello", Phase: scion.PhaseRunning, ContainerStatus: "Up 3 hours"}, &fakeGuestBoot{current: bootB, recorded: bootA}, nil, false},
		{"same boot", running, &fakeGuestBoot{current: bootA, recorded: bootA}, podmanSays(false), false},
		{"nothing recorded", running, &fakeGuestBoot{current: bootB}, podmanSays(false), false},
		{"boot unreadable", running, &fakeGuestBoot{recorded: bootA, currentErr: errors.New("vm")}, podmanSays(false), false},
		{"no tracker", running, nil, podmanSays(false), false},
		{"suspended record", &scion.Agent{Slug: "hello", Phase: scion.PhaseSuspended}, &fakeGuestBoot{current: bootB, recorded: bootA}, podmanSays(false), false},
		{"no record", nil, &fakeGuestBoot{current: bootB, recorded: bootA}, podmanSays(false), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Deps{GuestBoot: c.gb, ContainerRunning: c.cr}
			if got := guestRestartedUnder(context.Background(), d, "/lever", "hello", c.rec); got != c.want {
				t.Errorf("got %t, want %t", got, c.want)
			}
		})
	}
}

func startedBefore(before bool, err error) func(context.Context, string) (bool, error) {
	return func(context.Context, string) (bool, error) { return before, err }
}

// With no boot id kept, podman's start time is the evidence of a VM restart:
// the container is not running and last started before the current boot.
func TestGuestRestartedUnderWithoutRecord(t *testing.T) {
	running := &scion.Agent{Slug: "hello", Phase: scion.PhaseRunning, ContainerStatus: "created"}
	noRecord := &fakeGuestBoot{current: bootB}
	cases := []struct {
		name   string
		rec    *scion.Agent
		cr     func(context.Context, string) (bool, error)
		before func(context.Context, string) (bool, error)
		want   bool
	}{
		{"stopped, started in an earlier boot", running, podmanSays(false), startedBefore(true, nil), true},
		{"stopped, started in this boot", running, podmanSays(false), startedBefore(false, nil), false},
		{"running, started in an earlier boot", running, podmanSays(true), startedBefore(true, nil), false},
		{"start time unreadable", running, podmanSays(false), startedBefore(true, errors.New("podman")), false},
		{"no start probe", running, podmanSays(false), nil, false},
		{"no podman", running, nil, startedBefore(true, nil), false},
		{"podman fails", running, func(context.Context, string) (bool, error) { return false, errors.New("x") }, startedBefore(true, nil), false},
		{"suspended record", &scion.Agent{Slug: "hello", Phase: scion.PhaseSuspended}, podmanSays(false), startedBefore(true, nil), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var asked []string
			before := c.before
			if before != nil {
				inner := before
				before = func(ctx context.Context, ref string) (bool, error) {
					asked = append(asked, ref)
					return inner(ctx, ref)
				}
			}
			d := Deps{GuestBoot: noRecord, ContainerRunning: c.cr, ContainerStartedBeforeBoot: before}
			if got := guestRestartedUnder(context.Background(), d, "/lever", "hello", c.rec); got != c.want {
				t.Errorf("got %t, want %t", got, c.want)
			}
			for _, ref := range asked {
				if ref != "lever--hello" {
					t.Errorf("asked about %q, want the manager's container", ref)
				}
			}
		})
	}
	// A kept boot id decides alone: the same boot is never a restart,
	// whatever podman's start time says.
	d := Deps{GuestBoot: &fakeGuestBoot{current: bootA, recorded: bootA}, ContainerRunning: podmanSays(false), ContainerStartedBeforeBoot: startedBefore(true, nil)}
	if guestRestartedUnder(context.Background(), d, "/lever", "hello", running) {
		t.Error("a kept boot id that did not change must not be overridden by the start time")
	}
}

// No boot id kept and the VM restarted under the manager: apply suspends and
// resumes it and records the boot.
func TestStartManagerRecoversWithoutRecord(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "created"}
	gb := &fakeGuestBoot{current: bootB}
	deps := Deps{
		Scion:                      scion.New(r, scion.Options{}),
		GuestBoot:                  gb,
		ContainerRunning:           podmanSays(false),
		ContainerStartedBeforeBoot: startedBefore(true, nil),
	}
	if err := runApply(app, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.suspendCalls != 1 || r.resumeCalls != 1 || r.startCalls != 0 || r.deleteCalls != 0 {
		t.Errorf("suspend=%d resume=%d start=%d delete=%d, want 1/1/0/0", r.suspendCalls, r.resumeCalls, r.startCalls, r.deleteCalls)
	}
	if len(gb.records) != 1 || gb.records[0] != bootB {
		t.Errorf("records = %v, want the current boot", gb.records)
	}
}

func TestRecordGuestBootSkipsUnchanged(t *testing.T) {
	gb := &fakeGuestBoot{current: bootA, recorded: bootA}
	recordGuestBoot(context.Background(), Deps{GuestBoot: gb})
	if len(gb.records) != 0 {
		t.Errorf("an unchanged boot must not be rewritten: %v", gb.records)
	}
	gb.current = bootB
	recordGuestBoot(context.Background(), Deps{GuestBoot: gb})
	if len(gb.records) != 1 || gb.recorded != bootB {
		t.Errorf("records = %v", gb.records)
	}
	recordGuestBoot(context.Background(), Deps{}) // nil tracker: no panic
}
