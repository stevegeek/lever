package apply

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/testutil"
)

// fakeGuestBoot is an in-memory GuestBoot.
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

// TestStartManagerRecoversAfterGuestRestart is lever#179's incident: the VM
// was killed hard and restarted, the hub record still says running, its
// container is "created" and never started. apply suspends and resumes it
// (keeping the conversation) instead of refusing, and records the new boot.
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
		// without podman's answer nothing is suspended (lever#179 review).
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

// RecoverAfterGuestRestart is up's half: it suspends and says so only once
// the record reads suspended.
func TestRecoverAfterGuestRestartForUp(t *testing.T) {
	_, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "created"}
	d := Deps{Scion: scion.New(r, scion.Options{}), GuestBoot: &fakeGuestBoot{current: bootB, recorded: bootA}, ContainerRunning: podmanSays(false)}
	if !RecoverAfterGuestRestart(context.Background(), d, "hello", "/lever") {
		t.Fatal("want the recovery")
	}
	if r.suspendCalls != 1 || r.resumeCalls != 0 {
		t.Errorf("suspend=%d resume=%d: up resumes on its own path", r.suspendCalls, r.resumeCalls)
	}

	_, f2 := newObserveFirstApp(t)
	r2 := &agentLifecycleRunner{FakeRunner: f2, slug: "hello", initPhase: "running", initContainerStatus: "created", suspendErr: errors.New("hub said no")}
	var logged logSink
	d2 := Deps{Scion: scion.New(r2, scion.Options{}), GuestBoot: &fakeGuestBoot{current: bootB, recorded: bootA}, ContainerRunning: podmanSays(false), Log: logged.logf}
	if RecoverAfterGuestRestart(context.Background(), d2, "hello", "/lever") {
		t.Fatal("a suspend that did not take must leave up on its observe path")
	}
	if !strings.Contains(strings.Join(logged.lines, "\n"), "warning: suspend of manager") {
		t.Errorf("the failed suspend must be logged: %q", logged.lines)
	}

	_, f3 := newObserveFirstApp(t)
	r3 := &agentLifecycleRunner{FakeRunner: f3, slug: "hello", initPhase: "running", initContainerStatus: "Up 2 hours"}
	d3 := Deps{Scion: scion.New(r3, scion.Options{}), GuestBoot: &fakeGuestBoot{current: bootA, recorded: bootA}, ContainerRunning: podmanSays(true)}
	if RecoverAfterGuestRestart(context.Background(), d3, "hello", "/lever") || r3.suspendCalls != 0 {
		t.Fatal("a live manager is left alone")
	}
}

func TestRecordGuestBootSkipsUnchanged(t *testing.T) {
	gb := &fakeGuestBoot{current: bootA, recorded: bootA}
	RecordGuestBoot(context.Background(), Deps{GuestBoot: gb})
	if len(gb.records) != 0 {
		t.Errorf("an unchanged boot must not be rewritten: %v", gb.records)
	}
	gb.current = bootB
	RecordGuestBoot(context.Background(), Deps{GuestBoot: gb})
	if len(gb.records) != 1 || gb.recorded != bootB {
		t.Errorf("records = %v", gb.records)
	}
	RecordGuestBoot(context.Background(), Deps{}) // nil tracker: no panic
}
