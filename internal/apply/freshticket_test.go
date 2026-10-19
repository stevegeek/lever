package apply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/testutil"
)

// oldManagerPodman answers State from answers in turn (the last one
// repeats). It plays a hostile old manager polling the tree: at every look it
// records whether a ticket sat in the tree while podman still showed the
// container.
type oldManagerPodman struct {
	t       *testing.T
	tree    string
	answers []podmanAnswer
	removed []string
	// removeErr fails every removal (a container that started again).
	removeErr error
	// exposed: a ticket was in the tree while a container was there.
	exposed bool
}

type podmanAnswer struct {
	st  jail.ContainerState
	err error
}

func (p *oldManagerPodman) ticketStaged() bool {
	_, err := os.Lstat(filepath.Join(p.tree, ".lever", "bootstrap.json"))
	return err == nil
}

func (p *oldManagerPodman) State(_ context.Context, ref string) (jail.ContainerState, error) {
	if !strings.HasSuffix(ref, "--hello") {
		p.t.Errorf("State(%q): not the manager's container", ref)
	}
	a := p.answers[0]
	if len(p.answers) > 1 {
		p.answers = p.answers[1:]
	}
	if !errors.Is(a.err, jail.ErrNoContainer) && p.ticketStaged() {
		p.exposed = true
	}
	return a.st, a.err
}

func (p *oldManagerPodman) Remove(_ context.Context, id string) error {
	if p.ticketStaged() {
		p.exposed = true
	}
	p.removed = append(p.removed, id)
	return p.removeErr
}

var (
	oldRunning = podmanAnswer{st: jail.ContainerState{ID: staleID, Name: "hello--hello", Status: "running", Running: true}}
	oldExited  = podmanAnswer{st: jail.ContainerState{ID: staleID, Name: "hello--hello", Status: "exited"}}
	oldGone    = podmanAnswer{err: jail.ErrNoContainer}
)

type freshTicketRun struct {
	err     error
	mints   int
	rearms  int
	starts  int
	deletes int
	ticket  string // the staged ticket after the run, "" when none
	// atBackup and atDelete: a ticket was in the tree at the backup, or when
	// the delete was sent, both while the old container still ran.
	atBackup, atDelete bool
}

// runFreshOverRunning runs `lever up --fresh` over a running manager whose
// container podman reports through pm.
func runFreshOverRunning(t *testing.T, pm *oldManagerPodman, unwired bool) freshTicketRun {
	t.Helper()
	app, f := newObserveFirstApp(t)
	pm.t, pm.tree = t, app.Tree
	r := &deleteSpyRunner{agentLifecycleRunner: agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "Up 4 days"}, pm: pm}
	var res freshTicketRun
	d := Deps{
		Scion: scion.New(r, scion.Options{}),
		Log:   func(string, ...any) {},
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) {
			res.mints++
			return BootstrapMaterial{Ticket: "for-the-new-manager"}, nil
		},
		RearmBootstrap: func(context.Context) error { res.rearms++; return nil },
		BackupManagerConversation: func(context.Context, *scion.Agent) error {
			res.atBackup = pm.ticketStaged()
			return nil
		},
		ContainerGoneRetry: RetryBudget{Attempts: 4, Interval: time.Millisecond},
	}
	if !unwired {
		d.StaleContainer = pm
	}
	res.err = Run(context.Background(), app, fillDeps(d), PlanOpts{Fresh: true})
	res.starts, res.deletes, res.atDelete = r.startCalls, r.deleteCalls, r.ticketAtDelete
	res.ticket = stagedTicket(t, app)
	return res
}

func stagedTicket(t *testing.T, app *config.App) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(app.Tree, ".lever", "bootstrap.json"))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// deleteSpyRunner notes whether a ticket was in the tree when the delete
// was sent.
type deleteSpyRunner struct {
	agentLifecycleRunner
	pm             *oldManagerPodman
	ticketAtDelete bool
}

func (r *deleteSpyRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "scion" && r.verb(args) == "delete" {
		r.ticketAtDelete = r.ticketAtDelete || r.pm.ticketStaged()
	}
	return r.agentLifecycleRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *deleteSpyRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// TestFreshNeverStagesTheTicketWhileTheOldContainerRuns: under --fresh the
// discarded manager shares the new one's CN and mounts the tree, so no
// ticket may sit there while podman shows its container. One that never goes
// away fails the run with no ticket minted or staged and nothing created.
func TestFreshNeverStagesTheTicketWhileTheOldContainerRuns(t *testing.T) {
	pm := &oldManagerPodman{answers: []podmanAnswer{oldRunning}}
	res := runFreshOverRunning(t, pm, false)
	testutil.WantErrContaining(t, res.err, "could not confirm that the previous manager's container")
	testutil.WantErrContaining(t, res.err, "state running")
	if pm.exposed || res.atBackup || res.atDelete || res.ticket != "" {
		t.Fatalf("a ticket was in the tree while the old container ran (exposed=%v backup=%v delete=%v staged=%q)", pm.exposed, res.atBackup, res.atDelete, res.ticket)
	}
	if res.mints != 0 || res.rearms != 0 || res.starts != 0 {
		t.Fatalf("mints=%d rearms=%d starts=%d, want 0/0/0", res.mints, res.rearms, res.starts)
	}
	if len(pm.removed) != 0 {
		t.Fatalf("removed %v: a running container is never removed", pm.removed)
	}
}

// TestFreshStagesTheTicketOnceTheContainerIsGone: the ticket is minted and
// staged only after podman shows the container gone, then the manager is
// created on it.
func TestFreshStagesTheTicketOnceTheContainerIsGone(t *testing.T) {
	pm := &oldManagerPodman{answers: []podmanAnswer{oldRunning, oldRunning, oldGone}}
	res := runFreshOverRunning(t, pm, false)
	if res.err != nil {
		t.Fatalf("Run: %v", res.err)
	}
	if pm.exposed || res.atBackup || res.atDelete {
		t.Fatalf("a ticket was in the tree while the old container ran (exposed=%v backup=%v delete=%v)", pm.exposed, res.atBackup, res.atDelete)
	}
	if res.mints != 1 || res.rearms != 0 || res.deletes != 1 || res.starts != 1 {
		t.Fatalf("mints=%d rearms=%d deletes=%d starts=%d, want 1/0/1/1", res.mints, res.rearms, res.deletes, res.starts)
	}
	if !strings.Contains(res.ticket, "for-the-new-manager") {
		t.Fatalf("staged %q, want this run's ticket", res.ticket)
	}
}

// A container with no process in it is removed by its id, then the ticket
// is staged once podman shows it gone.
func TestFreshRemovesAnExitedOldContainer(t *testing.T) {
	pm := &oldManagerPodman{answers: []podmanAnswer{oldExited, oldGone}}
	res := runFreshOverRunning(t, pm, false)
	if res.err != nil {
		t.Fatalf("Run: %v", res.err)
	}
	if len(pm.removed) != 1 || pm.removed[0] != staleID {
		t.Fatalf("removed %v, want the inspected id", pm.removed)
	}
	if pm.exposed || res.mints != 1 || res.starts != 1 {
		t.Fatalf("exposed=%v mints=%d starts=%d", pm.exposed, res.mints, res.starts)
	}
}

// An exited container that starts again during its removal is not removed
// (podman refuses without --force), and the run refuses.
func TestFreshRefusesAContainerThatRestartsDuringRemoval(t *testing.T) {
	pm := &oldManagerPodman{answers: []podmanAnswer{oldExited, oldRunning}, removeErr: errors.New("cannot remove container: container is running")}
	res := runFreshOverRunning(t, pm, false)
	testutil.WantErrContaining(t, res.err, "staged no enrolment ticket")
	if pm.exposed || res.ticket != "" || res.mints != 0 || res.starts != 0 {
		t.Fatalf("exposed=%v staged=%q mints=%d starts=%d, want no ticket and no create", pm.exposed, res.ticket, res.mints, res.starts)
	}
}

// An answer podman did not give (a failed inspect) is not a removal: the
// run refuses, and so does a run with no podman reader wired.
func TestFreshRefusesWhenTheContainerStateIsUnknown(t *testing.T) {
	pm := &oldManagerPodman{answers: []podmanAnswer{{err: errors.New("podman: cannot connect to the service")}}}
	res := runFreshOverRunning(t, pm, false)
	testutil.WantErrContaining(t, res.err, "unreadable: podman: cannot connect")
	if res.ticket != "" || res.mints != 0 || res.rearms != 0 || res.starts != 0 {
		t.Fatalf("staged=%q mints=%d rearms=%d starts=%d, want nothing", res.ticket, res.mints, res.rearms, res.starts)
	}

	res = runFreshOverRunning(t, &oldManagerPodman{answers: []podmanAnswer{oldGone}}, true)
	testutil.WantErrContaining(t, res.err, "podman in the jail is not wired")
	if res.ticket != "" || res.mints != 0 || res.starts != 0 {
		t.Fatalf("unwired: staged=%q mints=%d starts=%d, want nothing", res.ticket, res.mints, res.starts)
	}
}

// A latch already spent falls back to the re-arm, still after the
// container is gone.
func TestFreshRearmsASpentLatchAfterTheContainerIsGone(t *testing.T) {
	app, f := newObserveFirstApp(t)
	pm := &oldManagerPodman{t: t, tree: app.Tree, answers: []podmanAnswer{oldRunning, oldGone}}
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "running", initContainerStatus: "Up 4 days"}
	rearmed := false
	d := Deps{
		Scion:                scion.New(r, scion.Options{}),
		Log:                  func(string, ...any) {},
		StaleContainer:       pm,
		ContainerGoneRetry:   RetryBudget{Attempts: 4, Interval: time.Millisecond},
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) { return BootstrapMaterial{}, ErrBootstrapLatched },
		RearmBootstrap: func(context.Context) error {
			if len(pm.answers) != 1 {
				t.Error("re-armed before podman showed the container gone")
			}
			rearmed = true
			return nil
		},
	}
	if err := Run(context.Background(), app, fillDeps(d), PlanOpts{Fresh: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rearmed || r.startCalls != 1 {
		t.Fatalf("rearmed=%v starts=%d", rearmed, r.startCalls)
	}
}

// --fresh over no record still checks podman: a container the hub lost the
// record of is the old manager all the same.
func TestFreshWithNoRecordRefusesAnOrphanContainer(t *testing.T) {
	app, f := newObserveFirstApp(t)
	pm := &oldManagerPodman{t: t, tree: app.Tree, answers: []podmanAnswer{oldRunning}}
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello"}
	mints := 0
	d := Deps{
		Scion:                scion.New(r, scion.Options{}),
		Log:                  func(string, ...any) {},
		StaleContainer:       pm,
		ContainerGoneRetry:   RetryBudget{Attempts: 3, Interval: time.Millisecond},
		MintManagerBootstrap: func(context.Context) (BootstrapMaterial, error) { mints++; return BootstrapMaterial{Ticket: "t"}, nil },
	}
	err := Run(context.Background(), app, fillDeps(d), PlanOpts{Fresh: true})
	testutil.WantErrContaining(t, err, "staged no enrolment ticket")
	if pm.exposed || stagedTicket(t, app) != "" || mints != 0 || r.startCalls != 0 {
		t.Fatalf("exposed=%v mints=%d starts=%d, want no ticket and no create", pm.exposed, mints, r.startCalls)
	}
}

// The fresh start lever makes on its own for a manager with no conversation
// re-arms only once podman shows the deleted manager's container gone.
func TestFreshWithoutConversationWaitsForTheOldContainer(t *testing.T) {
	app, f := newObserveFirstApp(t)
	pm := &oldManagerPodman{t: t, tree: app.Tree, answers: []podmanAnswer{oldRunning}}
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "error", initContainerStatus: "Exited (1)", dieAfterLiveLists: 2}
	rearms := 0
	err := Run(context.Background(), app, fillDeps(Deps{
		Scion:              scion.New(r, scion.Options{}),
		StaleContainer:     pm,
		ContainerGoneRetry: RetryBudget{Attempts: 3, Interval: time.Millisecond},
		ManagerLiveRetry:   RetryBudget{Attempts: 3, Interval: time.Millisecond, Settle: 10 * time.Second},
		HarnessLogTail:     func(context.Context, string) (string, error) { return noConvLog, nil },
		RearmBootstrap:     func(context.Context) error { rearms++; return nil },
		ManagerConversationEmpty: func(context.Context) error {
			return nil
		},
	}), PlanOpts{FreshWithoutConversation: true})
	testutil.WantErrContaining(t, err, "staged no enrolment ticket")
	if r.deleteCalls != 1 || r.startCalls != 0 || rearms != 0 {
		t.Fatalf("deletes=%d starts=%d rearms=%d, want the delete and nothing after it", r.deleteCalls, r.startCalls, rearms)
	}
}
