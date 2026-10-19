package apply

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
)

// verbSpy records the changes EnsureManager makes to the manager, in order,
// and can make every list fail.
type verbSpy struct {
	ManagerScion
	// r is the record the fake hub keeps; a stop lands on it.
	r       *agentLifecycleRunner
	listErr error
	// listErrUntilDelete: listErr ends once a delete succeeds (the oversized
	// record was the manager's own).
	listErrUntilDelete bool
	verbs              []string
}

func (s *verbSpy) note(v string, err error) error {
	if err == nil {
		s.verbs = append(s.verbs, v)
	} else {
		s.verbs = append(s.verbs, v+"(failed)")
	}
	return err
}

func (s *verbSpy) List(ctx context.Context, project string) ([]scion.Agent, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.ManagerScion.List(ctx, project)
}

func (s *verbSpy) Start(ctx context.Context, o scion.StartOpts) error {
	return s.note("create", s.ManagerScion.Start(ctx, o))
}

func (s *verbSpy) Resume(ctx context.Context, w, p string) error {
	return s.note("resume", s.ManagerScion.Resume(ctx, w, p))
}

func (s *verbSpy) ResumeForce(ctx context.Context, w, p string) error {
	return s.note("resume --force", s.ManagerScion.ResumeForce(ctx, w, p))
}

func (s *verbSpy) ResumeTask(ctx context.Context, w, p, task string) error {
	return s.note("resume-task", s.ManagerScion.ResumeTask(ctx, w, p, task))
}

func (s *verbSpy) Suspend(ctx context.Context, w, p string) error {
	return s.note("suspend", s.ManagerScion.Suspend(ctx, w, p))
}

func (s *verbSpy) Stop(ctx context.Context, w, p string) error {
	err := s.ManagerScion.Stop(ctx, w, p)
	if err == nil {
		s.r.phase, s.r.containerStatus = scion.PhaseStopped, "stopped"
	}
	return s.note("stop", err)
}

func (s *verbSpy) Delete(ctx context.Context, w, p string) error {
	err := s.ManagerScion.Delete(ctx, w, p)
	if err == nil && s.listErrUntilDelete {
		s.listErr = nil
	}
	return s.note("delete", err)
}

func (s *verbSpy) ResetAuth(ctx context.Context, a, p string) error {
	return s.note("reset-auth", s.ManagerScion.ResetAuth(ctx, a, p))
}

// gateWorld is what a gate row sets up around the manager's record.
type gateWorld struct {
	phase, container, image string
	roleErr                 error
	listTooLarge            bool
	// rebooted: the guest booted anew since the manager last ran live, and
	// podman shows its container not running.
	rebooted bool
}

// runGate runs col's request over w and renders what EnsureManager did: its
// changes and side effects in order, then "ok" or "refused".
func runGate(t *testing.T, col plannerColumn, w gateWorld) string {
	t.Helper()
	app, f := newObserveFirstApp(t)
	f.Script("scion reset-auth", proc.Result{Stdout: "ok"})
	f.Script("scion stop", proc.Result{Stdout: "ok"})
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: w.phase, initContainerStatus: w.container, image: w.image}
	spy := &verbSpy{ManagerScion: scion.New(r, scion.Options{}), r: r}
	d := fillDeps(Deps{Scion: scion.New(r, scion.Options{}), Log: func(string, ...any) {},
		InspectContainerMounts: func(context.Context, string) ([]jail.Mount, error) { return nil, nil },
		CheckLiveMounts:        func(context.Context, string, []jail.LiveMount) ([]jail.LiveMountProblem, error) { return nil, nil },
		RearmBootstrap:         func(context.Context) error { spy.verbs = append(spy.verbs, "ticket"); return nil },
		BackupManagerConversation: func(context.Context, *scion.Agent) error {
			spy.verbs = append(spy.verbs, "backup")
			return nil
		},
	})
	d.ManagerVerbs = spy
	d.ManagerPhaseWait = fastRetry(20)
	d.VerifyAgentRole = func(context.Context, string, string) error { return w.roleErr }
	if col.q.Intent == ManagerFresh {
		d = withContainerGone(d)
	}
	if w.rebooted {
		d.GuestBoot = &fakeGuestBoot{current: bootB, recorded: bootA}
		d.ContainerRunning = podmanSays(false)
	}
	if w.listTooLarge {
		spy.listErr = fmt.Errorf("scion list: %w", scion.ErrAgentListTooLarge)
		spy.listErrUntilDelete = col.q.Intent == ManagerFresh
	}
	q := col.q
	q.WhileSuspended = func(context.Context) error { spy.verbs = append(spy.verbs, "swap"); return nil }
	q.BeginSession = func() (func() error, error) { return func() error { return nil }, nil }
	q.StageTicket = func(context.Context) error { spy.verbs = append(spy.verbs, "ticket"); return nil }
	_, err := EnsureManager(context.Background(), app, d, q)
	end := "ok"
	if err != nil {
		end = "refused"
	}
	return strings.Join(append(spy.verbs, end), " ")
}

// gateColumns are the callers the gate table runs: up and apply share one
// column, since the gates before the plan are the same for both.
var gateColumns = []plannerColumn{
	{"up", ManagerRequest{Intent: ManagerKeep, Subject: "up"}},
	{"fresh", ManagerRequest{Intent: ManagerFresh}},
	{"new", ManagerRequest{Intent: ManagerNewSession, Task: "boot"}},
	{"restore", ManagerRequest{Intent: ManagerRestore}},
	{"park", ManagerRequest{Intent: ManagerPark}},
	{"reenrol", ManagerRequest{Intent: ManagerReenrol}},
	{"token", ManagerRequest{Intent: ManagerTokenReset}},
}

// TestPlannerGates runs EnsureManager itself for what the planner does not
// see: a stale record (created on an unqualified image name), a pre-role
// record (VerifyAgentRole refuses it), an agent list too large to read and
// a guest that booted anew under a running record. Each cell is the changes
// made, in order ("ticket" is a staged enrolment ticket, "swap" the caller's
// work while suspended), then ok or refused. A change here is a behaviour
// change, to make on purpose.
func TestPlannerGates(t *testing.T) {
	const unqualified = "scionlocal/lever-claude:arm64"
	preRole := errors.New("stored role reads as full")
	rows := []struct {
		name string
		w    gateWorld
		want []string // in gateColumns' order
	}{
		{"running", gateWorld{phase: "running", container: "Up 5 minutes"}, []string{
			"ok", "suspend backup delete ticket create ok", "suspend swap stop resume-task ok", "suspend swap resume ok",
			"suspend ok", "ticket suspend resume ok", "reset-auth ok"}},
		{"suspended", gateWorld{phase: "suspended", container: "stopped"}, []string{
			"ticket resume ok", "backup delete ticket create ok", "swap stop resume-task ok", "swap resume ok",
			"suspend ok", "ticket resume ok", "ok"}},
		// A stale record is refused before any change by every path that
		// resumes it; --fresh is the remedy. The token reset and the park
		// start nothing, so the hub's image rewrite never applies to them.
		{"stale-running", gateWorld{phase: "running", container: "Up 5 minutes", image: unqualified}, []string{
			"refused", "suspend backup delete ticket create ok", "refused", "refused",
			"suspend ok", "refused", "reset-auth ok"}},
		{"stale-suspended", gateWorld{phase: "suspended", container: "stopped", image: unqualified}, []string{
			"refused", "backup delete ticket create ok", "refused", "refused",
			"suspend ok", "refused", "ok"}},
		// A pre-role record is never resumed and its token never reset. up and
		// the broker refuse it before any change; a new conversation and a
		// restore read the role only once the conversation was saved or
		// swapped, and leave the manager stopped or suspended.
		{"pre-role-running", gateWorld{phase: "running", container: "Up 5 minutes", roleErr: preRole}, []string{
			"refused", "suspend backup delete ticket create ok", "suspend swap stop refused", "suspend swap refused",
			"suspend ok", "refused", "refused"}},
		{"pre-role-suspended", gateWorld{phase: "suspended", container: "stopped", roleErr: preRole}, []string{
			"refused", "backup delete ticket create ok", "swap stop refused", "swap refused",
			"suspend ok", "refused", "ok"}},
		// Only --fresh (which deletes by name) and the park (which suspends
		// by name) act on a record the list hides.
		{"oversized-list", gateWorld{phase: "running", container: "Up 5 minutes", listTooLarge: true}, []string{
			"refused", "suspend backup delete ticket create ok", "refused", "refused",
			"suspend ok", "refused", "refused"}},
		// The guest's boot id is read by up and apply alone: they suspend the
		// record over the dead container and resume it on its conversation.
		{"guest-rebooted", gateWorld{phase: "running", container: "created", rebooted: true}, []string{
			"suspend ticket resume ok", "suspend backup delete ticket create ok", "suspend swap stop resume-task ok", "suspend swap resume ok",
			"suspend ok", "ticket suspend resume ok", "ok"}},
	}
	for _, row := range rows {
		if len(row.want) != len(gateColumns) {
			t.Fatalf("%s: %d columns, want %d", row.name, len(row.want), len(gateColumns))
		}
		for i, col := range gateColumns {
			t.Run(row.name+"/"+col.name, func(t *testing.T) {
				got := runGate(t, col, row.w)
				if got != row.want[i] {
					t.Errorf("got %q, want %q", got, row.want[i])
				}
				// A refusal never leaves a ticket staged, a record deleted or a
				// manager started.
				if strings.HasSuffix(got, "refused") {
					for _, v := range []string{"ticket", "delete", "create", "resume"} {
						if strings.Contains(got, v) {
							t.Errorf("refused after %s: %q", v, got)
						}
					}
				}
			})
		}
	}
}
