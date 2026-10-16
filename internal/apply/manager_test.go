package apply

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/testutil"
)

func describePlan(p managerPlan, err error) string {
	switch {
	case err != nil:
		return "refuse"
	case p.recover:
		return "recover"
	case p.verb == verbNone:
		return "keep"
	case p.mustCreate:
		return p.verb.String() + "!"
	}
	return p.verb.String()
}

// TestPlanManagerKeepAndFresh is planManager over the shared observation set:
// the same decisions TestStartManagerDecisions pins through the whole step,
// as a pure table. The hub's container column decides nothing.
func TestPlanManagerKeepAndFresh(t *testing.T) {
	want := map[string][2]string{
		"absent":            {"create", "create!"},
		"running":           {"keep", "delete+create"},
		"running-exited":    {"keep", "delete+create"},
		"running-created":   {"keep", "delete+create"},
		"running-no-status": {"keep", "delete+create"},
		"suspended":         {"resume", "delete+create"},
		"stopped":           {"resume", "delete+create"},
		"error":             {"resume --force", "delete+create"},
		"created":           {"refuse", "delete+create"},
		"provisioning":      {"refuse", "delete+create"},
		"cloning":           {"refuse", "delete+create"},
		"starting":          {"refuse", "delete+create"},
		"stopping":          {"refuse", "delete+create"},
		"resumed":           {"refuse", "delete+create"},
		"unknown":           {"refuse", "delete+create"},
	}
	for _, o := range testutil.ManagerObservations {
		w, ok := want[o.Name]
		if !ok {
			t.Errorf("%s: the observation set grew; pin the plan for it", o.Name)
			continue
		}
		obs := managerObservation{present: o.Phase != "", phase: o.Phase, container: o.Container}
		for i, intent := range []ManagerIntent{ManagerKeep, ManagerFresh} {
			p, err := planManager(ManagerRequest{Intent: intent}, "m", obs)
			if got := describePlan(p, err); got != w[i] {
				t.Errorf("%s %s: %s, want %s (%v)", o.Name, intent, got, w[i], err)
			}
		}
	}
	// podman's word on a running record's container: recover once, then keep
	// whatever the next reading shows, so a suspend that did not take is
	// refused by the gate rather than tried again.
	for _, o := range []managerObservation{
		{present: true, phase: scion.PhaseRunning, restarted: true},
		{present: true, phase: scion.PhaseRunning, containerDown: true},
	} {
		if p, err := planManager(ManagerRequest{Intent: ManagerKeep}, "m", o); describePlan(p, err) != "recover" {
			t.Errorf("%+v: %s, want recover", o, describePlan(p, err))
		}
		o.recovered = true
		if p, err := planManager(ManagerRequest{Intent: ManagerKeep}, "m", o); describePlan(p, err) != "keep" {
			t.Errorf("%+v: %s, want keep", o, describePlan(p, err))
		}
		if p, err := planManager(ManagerRequest{Intent: ManagerFresh}, "m", o); describePlan(p, err) != "delete+create" {
			t.Errorf("fresh %+v: %s", o, describePlan(p, err))
		}
	}
}

// TestEnsureManagerTakesTheLock: no lock wired refuses before anything is
// read; a held lock is the caller's answer, unchanged; the lock is released
// when EnsureManager returns.
func TestEnsureManagerTakesTheLock(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "Up 1 minute"}
	d := fillDeps(Deps{Scion: scion.New(r, scion.Options{}), Log: func(string, ...any) {}})
	d.ManagerLock = nil
	if _, err := EnsureManager(context.Background(), app, d, ManagerRequest{Intent: ManagerKeep}); err == nil || !strings.Contains(err.Error(), "no lifecycle lock") {
		t.Fatalf("no lock: %v", err)
	}
	busy := errors.New("`lever up` is changing this instance's manager")
	var asked string
	d.ManagerLock = func(what string) (func(), error) { asked = what; return nil, busy }
	if _, err := EnsureManager(context.Background(), app, d, ManagerRequest{Intent: ManagerKeep, What: "the test"}); !errors.Is(err, busy) || asked != "the test" {
		t.Fatalf("busy: %v (asked %q)", err, asked)
	}
	if r.listCalls != 0 {
		t.Fatalf("%d list(s) without the lock", r.listCalls)
	}
	held, released := false, false
	d.ManagerLock = func(string) (func(), error) {
		held = true
		return func() { released = true }, nil
	}
	if _, err := EnsureManager(context.Background(), app, d, ManagerRequest{Intent: ManagerKeep}); err != nil {
		t.Fatal(err)
	}
	if !held || !released {
		t.Fatalf("held %v released %v", held, released)
	}
}

// TestKeepRecoversARunningRecordOverAGoneContainer: a record the hub reads
// running over a container podman shows exited or gone is suspended and
// resumed on its conversation; one podman shows created (a start under way)
// or running, or cannot be read, is kept and gated as it is.
func TestKeepRecoversARunningRecordOverAGoneContainer(t *testing.T) {
	for _, c := range []struct {
		name    string
		j       *fakeJanitor
		recover bool
	}{
		{"exited", &fakeJanitor{states: []jail.ContainerState{{ID: staleID, Status: "exited"}}}, true},
		{"gone", &fakeJanitor{stateErr: jail.ErrNoContainer}, true},
		{"created", &fakeJanitor{states: []jail.ContainerState{created(staleID)}}, false},
		{"running", &fakeJanitor{states: []jail.ContainerState{{ID: staleID, Status: "running", Running: true}}}, false},
		{"unreadable", &fakeJanitor{stateErr: errors.New("podman hung")}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, f := newObserveFirstApp(t)
			r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "Exited (1) 2 seconds ago"}
			d := fillDeps(Deps{Scion: scion.New(r, scion.Options{}), StaleContainer: c.j, Log: func(string, ...any) {}})
			res, err := EnsureManager(context.Background(), app, d, ManagerRequest{Intent: ManagerKeep})
			if c.recover {
				if err != nil || r.suspendCalls != 1 || r.resumeCalls != 1 || r.deleteCalls != 0 || res.Verb != "resume" {
					t.Fatalf("err %v suspend %d resume %d delete %d verb %q", err, r.suspendCalls, r.resumeCalls, r.deleteCalls, res.Verb)
				}
				return
			}
			if err == nil || r.suspendCalls != 0 || r.resumeCalls != 0 {
				t.Fatalf("err %v suspend %d resume %d: want it kept and refused by the gate", err, r.suspendCalls, r.resumeCalls)
			}
		})
	}
	// A suspend that does not take leaves the record running: it is not
	// tried again, the gate refuses it.
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "Exited (1)", suspendErr: errors.New("hub said no")}
	j := &fakeJanitor{stateErr: jail.ErrNoContainer}
	d := fillDeps(Deps{Scion: scion.New(r, scion.Options{}), StaleContainer: j, Log: func(string, ...any) {}})
	if _, err := EnsureManager(context.Background(), app, d, ManagerRequest{Intent: ManagerKeep}); err == nil || r.suspendCalls != 1 || r.resumeCalls != 0 {
		t.Fatalf("err %v suspend %d resume %d", err, r.suspendCalls, r.resumeCalls)
	}
}

// TestKeepStagesATicketOnlyToStartOrResume: up's keep path passes whether
// its broker-up staged a ticket; a resume or create without one re-arms the
// latch once, a kept running manager needs none.
func TestKeepStagesATicketOnlyToStartOrResume(t *testing.T) {
	for _, c := range []struct {
		phase, container string
		staged           bool
		rearms           int
	}{
		{"suspended", "stopped", false, 1},
		{"suspended", "stopped", true, 0},
		{"", "", false, 1},
		{"running", "Up 1 minute", false, 0},
	} {
		app, f := newObserveFirstApp(t)
		r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: c.phase, initContainerStatus: c.container}
		rearms := 0
		d := fillDeps(Deps{Scion: scion.New(r, scion.Options{}), Log: func(string, ...any) {},
			RearmBootstrap: func(context.Context) error { rearms++; return nil }})
		if _, err := EnsureManager(context.Background(), app, d, ManagerRequest{Intent: ManagerKeep, Subject: "up", TicketStaged: c.staged}); err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if rearms != c.rearms {
			t.Errorf("%+v: %d re-arm(s), want %d", c, rearms, c.rearms)
		}
	}
}

// TestPlanManagerRestore: a restore reads the phase alone (the shared
// observation set, as TestConversationDecisions pins it through the command).
func TestPlanManagerRestore(t *testing.T) {
	want := map[string]string{"absent": "refuse", "running": "suspend,resume", "running-exited": "suspend,resume",
		"running-created": "suspend,resume", "running-no-status": "suspend,resume", "suspended": "resume"}
	for _, o := range testutil.ManagerObservations {
		w, ok := want[o.Name]
		if !ok {
			w = "refuse"
		}
		p, err := planManager(ManagerRequest{Intent: ManagerRestore}, "m", managerObservation{present: o.Phase != "", phase: o.Phase, container: o.Container})
		got := describePlan(p, err)
		if err == nil && p.suspend {
			got = "suspend," + got
		}
		if got != w {
			t.Errorf("%s: %s, want %s (%v)", o.Name, got, w, err)
		}
	}
}

// TestFreshBackupSuspendsAndResumesOnFailure: --fresh suspends a running
// manager before its backup, so the copy is the conversation as it ends; a
// failed backup resumes it through the role guard and its mount checks and
// deletes nothing, and a guard refusal leaves it suspended.
func TestFreshBackupSuspendsAndResumesOnFailure(t *testing.T) {
	boom := errors.New("no home")
	for _, c := range []struct {
		name    string
		roleErr error
		resumes int
		want    string
	}{
		{"resumed", nil, 1, "the manager was resumed"},
		{"guard refuses", errors.New("pre-role record"), 0, "the manager stays suspended: pre-role record"},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, f := newObserveFirstApp(t)
			r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "Up 1 minute"}
			var order []string
			d := fillDeps(Deps{Scion: scion.New(r, scion.Options{}), Log: func(string, ...any) {},
				InspectContainerMounts: func(context.Context, string) ([]jail.Mount, error) { return nil, nil },
				CheckLiveMounts:        func(context.Context, string, []jail.LiveMount) ([]jail.LiveMountProblem, error) { return nil, nil },
				BackupManagerConversation: func(context.Context, *scion.Agent) error {
					order = append(order, fmt.Sprintf("backup after %d suspend(s)", r.suspendCalls))
					return boom
				},
			})
			d.VerifyAgentRole = func(context.Context, string, string) error { return c.roleErr }
			_, err := EnsureManager(context.Background(), app, d, ManagerRequest{Intent: ManagerFresh})
			if !errors.Is(err, boom) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if strings.Join(order, ",") != "backup after 1 suspend(s)" || r.resumeCalls != c.resumes || r.deleteCalls != 0 || r.startCalls != 0 {
				t.Fatalf("order %v resume %d delete %d start %d", order, r.resumeCalls, r.deleteCalls, r.startCalls)
			}
		})
	}
}

// TestPlanManagerNewSession: the phase and the agent's activity decide; a
// busy manager is left alone unless forced, and RunningOnly leaves any
// manager that is not running alone.
func TestPlanManagerNewSession(t *testing.T) {
	plan := func(q ManagerRequest, o managerObservation) string {
		q.Intent = ManagerNewSession
		p, err := planManager(q, "m", o)
		switch {
		case errors.Is(err, ErrManagerBusy):
			return "busy"
		case errors.Is(err, ErrManagerNotRunning):
			return "not-running"
		case err != nil:
			return "refuse"
		}
		return fmt.Sprintf("suspend=%v stop=%v %s", p.suspend, p.stop, p.verb)
	}
	running := managerObservation{present: true, phase: scion.PhaseRunning, container: "Up 1 minute"}
	busy := running
	busy.activity = scion.ActivityWorking
	suspended := managerObservation{present: true, phase: scion.PhaseSuspended}
	errored := managerObservation{present: true, phase: scion.PhaseError}
	for _, c := range []struct {
		q    ManagerRequest
		o    managerObservation
		want string
	}{
		{ManagerRequest{}, running, "suspend=true stop=true stop+resume"},
		{ManagerRequest{}, busy, "busy"},
		{ManagerRequest{Force: true}, busy, "suspend=true stop=true stop+resume"},
		{ManagerRequest{RunningOnly: true}, busy, "busy"},
		{ManagerRequest{}, suspended, "suspend=false stop=true stop+resume"},
		{ManagerRequest{RunningOnly: true}, suspended, "not-running"},
		{ManagerRequest{}, errored, "refuse"},
		{ManagerRequest{RunningOnly: true}, errored, "not-running"},
		{ManagerRequest{}, managerObservation{}, "refuse"},
		{ManagerRequest{RunningOnly: true}, managerObservation{}, "refuse"},
	} {
		if got := plan(c.q, c.o); got != c.want {
			t.Errorf("%+v over %+v: %s, want %s", c.q, c.o, got, c.want)
		}
	}
}

// TestPlanManagerPark: the suspend is sent whatever the reading (a power-off
// follows); only a stopped record is healed first.
func TestPlanManagerPark(t *testing.T) {
	for _, o := range testutil.ManagerObservations {
		p, err := planManager(ManagerRequest{Intent: ManagerPark}, "m", managerObservation{present: o.Phase != "", phase: o.Phase, container: o.Container})
		if err != nil || p.verb != verbSuspend || p.heal != (o.Phase == scion.PhaseStopped) {
			t.Errorf("%s: %+v %v", o.Name, p, err)
		}
	}
}

// reenrolDeps is the broker's wiring of the lifecycle over r: the mount
// checks pass, the gate takes one look.
func reenrolDeps(r *agentLifecycleRunner, staged *int) (Deps, ManagerRequest) {
	d := fillDeps(Deps{Scion: scion.New(r, scion.Options{}), Log: func(string, ...any) {},
		InspectContainerMounts: func(context.Context, string) ([]jail.Mount, error) { return nil, nil },
		CheckLiveMounts:        func(context.Context, string, []jail.LiveMount) ([]jail.LiveMountProblem, error) { return nil, nil },
	})
	d.VerifyAgentRole = func(context.Context, string, string) error { return nil }
	return d, ManagerRequest{Intent: ManagerReenrol, Subject: "broker",
		StageTicket: func(context.Context) error { *staged++; return nil }}
}

// TestReenrolBouncesByPhase: the broker's bounce stages a ticket and
// suspends and resumes a running manager, resumes a suspended or stopped
// one, forces an error one; it never creates or deletes, and refuses any
// other phase before the stage. The guest boot is recorded once the resumed
// container's mounts were read back.
func TestReenrolBouncesByPhase(t *testing.T) {
	for _, c := range []struct {
		phase, container string
		verb             string
		suspends, forces int
	}{
		{"running", "Up 5 minutes", "suspend+resume", 1, 0},
		{"suspended", "stopped", "resume", 0, 0},
		{"stopped", "stopped", "resume", 0, 0},
		{"error", "Exited (1)", "resume --force", 0, 1},
		{"starting", "", "", 0, 0},
		{"", "", "", 0, 0},
	} {
		t.Run(c.phase, func(t *testing.T) {
			app, f := newObserveFirstApp(t)
			r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: c.phase, initContainerStatus: c.container}
			staged := 0
			d, q := reenrolDeps(r, &staged)
			gb := &fakeGuestBoot{current: bootB}
			d.GuestBoot = gb
			res, err := EnsureManager(context.Background(), app, d, q)
			if c.verb == "" {
				if err == nil || staged != 0 || r.resumeCalls+r.suspendCalls != 0 || len(gb.records) != 0 {
					t.Fatalf("err %v staged %d resume %d suspend %d boots %v: want a refusal before the stage", err, staged, r.resumeCalls, r.suspendCalls, gb.records)
				}
				return
			}
			if err != nil || res.Verb != c.verb || staged != 1 || r.suspendCalls != c.suspends || r.resumeForceCalls != c.forces || r.resumeCalls != 1 {
				t.Fatalf("err %v verb %q staged %d suspend %d resume %d force %d", err, res.Verb, staged, r.suspendCalls, r.resumeCalls, r.resumeForceCalls)
			}
			if r.startCalls+r.deleteCalls != 0 || len(gb.records) != 1 {
				t.Fatalf("start %d delete %d boots %v", r.startCalls, r.deleteCalls, gb.records)
			}
		})
	}
}

// TestReenrolRefusals: what the lifecycle refuses for the broker is marked
// ErrManagerRefused and changes nothing; a revoke caught under the lock
// reads nothing at all; a resume that dies fails the gate.
func TestReenrolRefusals(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped"}
	staged := 0
	d, q := reenrolDeps(r, &staged)
	d.VerifyAgentRole = func(context.Context, string, string) error { return errors.New("stored role reads as full") }
	if _, err := EnsureManager(context.Background(), app, d, q); !errors.Is(err, ErrManagerRefused) || staged != 0 || r.resumeCalls != 0 {
		t.Fatalf("role: err %v staged %d resume %d", err, staged, r.resumeCalls)
	}

	d, q = reenrolDeps(r, &staged)
	app.Manager.ReadOnly = []string{"kb"} // a read_only entry the tree does not hold
	if _, err := EnsureManager(context.Background(), app, d, q); !errors.Is(err, ErrManagerRefused) || staged != 0 || r.resumeCalls != 0 {
		t.Fatalf("tree: err %v staged %d resume %d", err, staged, r.resumeCalls)
	}
	app.Manager.ReadOnly = nil

	revoked := errors.New("revoked")
	q.UnderLock = func() error { return revoked }
	lists := r.listCalls
	if _, err := EnsureManager(context.Background(), app, d, q); !errors.Is(err, revoked) || r.listCalls != lists || staged != 0 {
		t.Fatalf("under lock: err %v lists %d staged %d", err, r.listCalls-lists, staged)
	}

	app2, f2 := newObserveFirstApp(t)
	r2 := &agentLifecycleRunner{FakeRunner: f2, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped", liveWhenContainer: "Exited (1)"}
	d2, q2 := reenrolDeps(r2, &staged)
	gb := &fakeGuestBoot{current: bootB}
	d2.GuestBoot = gb
	if _, err := EnsureManager(context.Background(), app2, d2, q2); err == nil || errors.Is(err, ErrManagerRefused) {
		t.Fatalf("dead resume: err %v", err)
	}
}

// heldSpy notes whether the lifecycle lock was held at the manager's
// resume and at its last list.
type heldSpy struct {
	ManagerScion
	held                      *bool
	heldAtResume, heldAtLast  bool
	resumed, listsAfterResume int
}

func (s *heldSpy) Resume(ctx context.Context, worker, project string) error {
	s.resumed++
	s.heldAtResume = *s.held
	return s.ManagerScion.Resume(ctx, worker, project)
}

func (s *heldSpy) List(ctx context.Context, project string) ([]scion.Agent, error) {
	if s.resumed > 0 {
		s.listsAfterResume++
		s.heldAtLast = *s.held
	}
	return s.ManagerScion.List(ctx, project)
}

// TestReenrolReleasesTheLockBeforeTheGate: the broker's bounce holds the
// lifecycle lock through the resume and its mount read-back, and lets go
// before the liveness gate, which can wait minutes on claude.
func TestReenrolReleasesTheLockBeforeTheGate(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped"}
	staged := 0
	d, q := reenrolDeps(r, &staged)
	held := false
	d.ManagerLock = func(string) (func(), error) {
		held = true
		return func() { held = false }, nil
	}
	spy := &heldSpy{ManagerScion: d.Scion, held: &held}
	d.ManagerVerbs = spy
	gb := &fakeGuestBoot{current: bootB}
	d.GuestBoot = gb
	if _, err := EnsureManager(context.Background(), app, d, q); err != nil {
		t.Fatal(err)
	}
	if spy.resumed != 1 || !spy.heldAtResume || spy.listsAfterResume == 0 || spy.heldAtLast || held || len(gb.records) != 1 {
		t.Fatalf("resumes %d held at resume %v lists after %d held at the last %v held at the end %v boots %v",
			spy.resumed, spy.heldAtResume, spy.listsAfterResume, spy.heldAtLast, held, gb.records)
	}
}

// TestReenrolClearsTheManagersStopClaim: the bounce's resume goes through
// the stop-claim recovery, which removes the manager's own stopped container.
func TestReenrolClearsTheManagersStopClaim(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "suspended", initContainerStatus: "stopped",
		resumeErr: errors.New(stopClaimMsg), resumeFailsThenSucceed: 1}
	staged := 0
	d, q := reenrolDeps(r, &staged)
	j := &fakeJanitor{states: []jail.ContainerState{{ID: staleID, Name: "proj--m", Status: "exited"}}}
	d.StaleContainer = j
	d.StopClaimRetry = RetryBudget{Attempts: 3, Interval: time.Millisecond}
	if _, err := EnsureManager(context.Background(), app, d, q); err != nil || !j.did("rm ") || r.resumeCalls != 2 {
		t.Fatalf("err %v janitor %v resumes %d", err, j.calls, r.resumeCalls)
	}
}

// TestTokenResetUnderTheLock: the broker's reset of the manager's hub token
// reads the record again under the lock and resets only a running manager,
// behind the pre-role guard (no guard refuses).
func TestTokenResetUnderTheLock(t *testing.T) {
	for _, c := range []struct {
		phase, container string
		roleErr          error
		noGuard          bool
		want             string // "reset", "none" or "refused"
	}{
		{"running", "Up 1 hour", nil, false, "reset"},
		{"suspended", "stopped", nil, false, "none"},
		{"running", "Exited (1)", nil, false, "none"},
		{"running", "Up 1 hour", errors.New("stored role reads as full"), false, "refused"},
		{"running", "Up 1 hour", nil, true, "refused"},
	} {
		app, f := newObserveFirstApp(t)
		f.Script("scion reset-auth", proc.Result{Stdout: "ok"})
		r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: c.phase, initContainerStatus: c.container}
		d := fillDeps(Deps{Scion: scion.New(r, scion.Options{}), Log: func(string, ...any) {}})
		d.VerifyAgentRole = func(context.Context, string, string) error { return c.roleErr }
		if c.noGuard {
			d.VerifyAgentRole = nil
		}
		res, err := EnsureManager(context.Background(), app, d, ManagerRequest{Intent: ManagerTokenReset, Subject: "broker"})
		reset := sawScionCall(f, "reset-auth hello")
		switch c.want {
		case "reset":
			if err != nil || !reset || res.Verb != "reset-auth" {
				t.Errorf("%+v: err %v reset %v verb %q", c, err, reset, res.Verb)
			}
		case "none":
			if err != nil || reset || res.Verb != "" {
				t.Errorf("%+v: err %v reset %v verb %q", c, err, reset, res.Verb)
			}
		case "refused":
			if !errors.Is(err, ErrManagerRefused) || reset {
				t.Errorf("%+v: err %v reset %v", c, err, reset)
			}
		}
	}
}

// TestKeepNoCreateRefusesAGoneRecord: up's keep path ran none of the steps
// a create needs, so a record that vanished is refused, never created.
func TestKeepNoCreateRefusesAGoneRecord(t *testing.T) {
	p, err := planManager(ManagerRequest{Intent: ManagerKeep, NoCreate: true}, "m", managerObservation{})
	if err == nil || p.verb != verbNone || !strings.Contains(err.Error(), "nothing was created") {
		t.Fatalf("%+v %v", p, err)
	}
	if p, err := planManager(ManagerRequest{Intent: ManagerKeep, NoCreate: true}, "m", managerObservation{present: true, phase: scion.PhaseSuspended}); err != nil || p.verb != verbResume {
		t.Fatalf("suspended: %+v %v", p, err)
	}
}
