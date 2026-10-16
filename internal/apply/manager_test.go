package apply

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/jail"
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
