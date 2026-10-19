package apply

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/testutil"
)

// plannerCase is one row of the planner table: an observation of the
// manager, as planManager receives it.
type plannerCase struct {
	name string
	o    managerObservation
}

// plannerObservations is the shared observation set plus what only podman
// and the guest's boot id tell: a running record over a container that is
// down in this boot, and one over a guest that booted anew, each before and
// after this call recovered it once, and a running manager in a turn.
func plannerObservations() []plannerCase {
	var cs []plannerCase
	for _, o := range testutil.ManagerObservations {
		cs = append(cs, plannerCase{o.Name, managerObservation{present: o.Phase != "", phase: o.Phase, container: o.Container}})
	}
	running := managerObservation{present: true, phase: scion.PhaseRunning, container: "Up 6 seconds"}
	down, rebooted, busy := running, running, running
	down.container, down.containerDown = "Exited (1) 2 seconds ago", true
	rebooted.container, rebooted.restarted = "created", true
	busy.activity = scion.ActivityWorking
	downAgain, rebootedAgain := down, rebooted
	downAgain.recovered, rebootedAgain.recovered = true, true
	return append(cs,
		plannerCase{"running-container-down", down},
		plannerCase{"running-container-down-recovered", downAgain},
		plannerCase{"running-guest-rebooted", rebooted},
		plannerCase{"running-guest-rebooted-recovered", rebootedAgain},
		plannerCase{"running-busy", busy},
	)
}

// plannerColumn is one caller of EnsureManager: the request it sends.
type plannerColumn struct {
	name string
	q    ManagerRequest
}

// plannerColumns is every entry point that changes the manager. up and
// apply send the same request to the planner (they differ only in
// FreshWithoutConversation, which acts after a failed resume, not in the
// plan); keep-conversation is --fresh followed by a restore and has its own
// column below.
var plannerColumns = []plannerColumn{
	{"up", ManagerRequest{Intent: ManagerKeep, Subject: "up", FreshWithoutConversation: true}},
	{"apply", ManagerRequest{Intent: ManagerKeep}},
	{"fresh", ManagerRequest{Intent: ManagerFresh}},
	{"new", ManagerRequest{Intent: ManagerNewSession}},
	{"new-force", ManagerRequest{Intent: ManagerNewSession, Force: true}},
	{"daily", ManagerRequest{Intent: ManagerNewSession, RunningOnly: true}},
	{"restore", ManagerRequest{Intent: ManagerRestore}},
	{"park", ManagerRequest{Intent: ManagerPark}},
	{"reenrol", ManagerRequest{Intent: ManagerReenrol}},
	{"token", ManagerRequest{Intent: ManagerTokenReset}},
}

// describeFullPlan renders every action of a plan in the order EnsureManager
// takes them, or the refusal.
func describeFullPlan(p managerPlan, err error) string {
	switch {
	case errors.Is(err, ErrManagerBusy):
		return "busy"
	case errors.Is(err, ErrManagerNotRunning):
		return "not-running"
	case err != nil:
		return "refuse"
	case p.recover:
		return "recover"
	}
	var parts []string
	if p.heal {
		parts = append(parts, "heal")
	}
	if p.suspend {
		parts = append(parts, "suspend")
	}
	if p.stop {
		parts = append(parts, "stop")
	}
	switch {
	case p.verb == verbNone:
		parts = append(parts, "keep")
	case p.mustCreate:
		parts = append(parts, p.verb.String()+"!")
	default:
		parts = append(parts, p.verb.String())
	}
	return strings.Join(parts, ",")
}

// keepConversation is lever up --fresh --keep-conversation: the --fresh
// plan, then, when there was a record to back up, a restore over the
// manager the create left running.
func keepConversation(o managerObservation) string {
	fresh := describeFullPlan(planManager(ManagerRequest{Intent: ManagerFresh}, "m", o))
	if !o.present {
		return fresh
	}
	restore := describeFullPlan(planManager(ManagerRequest{Intent: ManagerRestore}, "m",
		managerObservation{present: true, phase: scion.PhaseRunning, container: "Up 1 second"}))
	return fresh + " then " + restore
}

// TestPlannerTable is the manager planner over every observation and every
// entry point, as one table: a decision one caller makes and another does
// not shows up side by side. A change here is a behaviour change, to make on
// purpose.
//
// Key: create! fails rather than keeps a record the create finds; recover
// suspends and plans again from a new reading; refuse changes nothing.
func TestPlannerTable(t *testing.T) {
	cols := []string{"up", "apply", "fresh", "keep-conversation", "new", "new-force", "daily", "restore", "park", "reenrol", "token"}
	const (
		fresh   = "delete+create"
		keepCnv = "delete+create then suspend,resume"
		newSess = "suspend,stop,stop+resume"
		restore = "suspend,resume"
		bounce  = "suspend,resume"
	)
	// Each row lists the columns in cols' order.
	want := map[string][]string{
		"absent":            {"create", "create", "create!", "create!", "refuse", "refuse", "refuse", "refuse", "suspend", "refuse", "keep"},
		"running":           {"keep", "keep", fresh, keepCnv, newSess, newSess, newSess, restore, "suspend", bounce, "reset-auth"},
		"running-exited":    {"keep", "keep", fresh, keepCnv, newSess, newSess, newSess, restore, "suspend", bounce, "keep"},
		"running-created":   {"keep", "keep", fresh, keepCnv, newSess, newSess, newSess, restore, "suspend", bounce, "keep"},
		"running-no-status": {"keep", "keep", fresh, keepCnv, newSess, newSess, newSess, restore, "suspend", bounce, "keep"},
		"suspended":         {"resume", "resume", fresh, keepCnv, "stop,stop+resume", "stop,stop+resume", "not-running", "resume", "suspend", "resume", "keep"},
		"stopped":           {"resume", "resume", fresh, keepCnv, "refuse", "refuse", "not-running", "refuse", "heal,suspend", "resume", "keep"},
		"error":             {"resume --force", "resume --force", fresh, keepCnv, "refuse", "refuse", "not-running", "refuse", "suspend", "resume --force", "keep"},
		"created":           {"refuse", "refuse", fresh, keepCnv, "refuse", "refuse", "not-running", "refuse", "suspend", "refuse", "keep"},
		"provisioning":      {"refuse", "refuse", fresh, keepCnv, "refuse", "refuse", "not-running", "refuse", "suspend", "refuse", "keep"},
		"cloning":           {"refuse", "refuse", fresh, keepCnv, "refuse", "refuse", "not-running", "refuse", "suspend", "refuse", "keep"},
		"starting":          {"refuse", "refuse", fresh, keepCnv, "refuse", "refuse", "not-running", "refuse", "suspend", "refuse", "keep"},
		"stopping":          {"refuse", "refuse", fresh, keepCnv, "refuse", "refuse", "not-running", "refuse", "suspend", "refuse", "keep"},
		"resumed":           {"refuse", "refuse", fresh, keepCnv, "refuse", "refuse", "not-running", "refuse", "suspend", "refuse", "keep"},
		"unknown":           {"refuse", "refuse", fresh, keepCnv, "refuse", "refuse", "not-running", "refuse", "suspend", "refuse", "keep"},
		// podman's word and the guest's boot id are read for up and apply
		// alone: every other caller plans from the phase as it is.
		"running-container-down":           {"recover", "recover", fresh, keepCnv, newSess, newSess, newSess, restore, "suspend", bounce, "keep"},
		"running-container-down-recovered": {"keep", "keep", fresh, keepCnv, newSess, newSess, newSess, restore, "suspend", bounce, "keep"},
		"running-guest-rebooted":           {"recover", "recover", fresh, keepCnv, newSess, newSess, newSess, restore, "suspend", bounce, "keep"},
		"running-guest-rebooted-recovered": {"keep", "keep", fresh, keepCnv, newSess, newSess, newSess, restore, "suspend", bounce, "keep"},
		"running-busy":                     {"keep", "keep", fresh, keepCnv, "busy", newSess, "busy", restore, "suspend", bounce, "reset-auth"},
	}
	byName := map[string]ManagerRequest{}
	for _, c := range plannerColumns {
		byName[c.name] = c.q
	}
	for _, c := range plannerObservations() {
		w, ok := want[c.name]
		if !ok {
			t.Errorf("%s: the observation set grew; pin every caller's plan for it", c.name)
			continue
		}
		if len(w) != len(cols) {
			t.Fatalf("%s: %d columns, want %d", c.name, len(w), len(cols))
		}
		for i, col := range cols {
			var got string
			if col == "keep-conversation" {
				got = keepConversation(c.o)
			} else {
				q, ok := byName[col]
				if !ok {
					t.Fatalf("no request for column %s", col)
				}
				got = describeFullPlan(planManager(q, "m", c.o))
			}
			if got != w[i] {
				t.Errorf("%s / %s: %s, want %s", c.name, col, got, w[i])
			}
		}
	}
}

// TestPlannerCoversEveryIntent: a new intent must get a column in the
// table, and an unknown one is refused.
func TestPlannerCoversEveryIntent(t *testing.T) {
	seen := map[ManagerIntent]bool{}
	for _, c := range plannerColumns {
		seen[c.q.Intent] = true
	}
	for i := ManagerKeep; i <= ManagerTokenReset; i++ {
		if !seen[i] {
			t.Errorf("intent %s has no column in the planner table", i)
		}
		if strings.HasPrefix(i.String(), "intent(") {
			t.Errorf("intent %d has no name", int(i))
		}
	}
	for _, i := range []ManagerIntent{0, ManagerTokenReset + 1} {
		if _, err := planManager(ManagerRequest{Intent: i}, "m", managerObservation{present: true, phase: scion.PhaseRunning}); err == nil {
			t.Errorf("intent %s: planned, want refused", i)
		}
	}
}

// TestPlannerProperties are the rules the table follows, over every row and
// column, so a new row cannot break them unnoticed.
func TestPlannerProperties(t *testing.T) {
	for _, c := range plannerObservations() {
		for _, col := range plannerColumns {
			p, err := planManager(col.q, "m", c.o)
			where := fmt.Sprintf("%s / %s", c.name, col.name)
			// A refusal acts on nothing.
			if err != nil && p != (managerPlan{}) {
				t.Errorf("%s: refused with actions %+v", where, p)
			}
			// Only --fresh deletes a record, and only up and apply recover.
			if p.verb == verbDeleteCreate && col.q.Intent != ManagerFresh {
				t.Errorf("%s: delete+create outside --fresh", where)
			}
			if p.recover && col.q.Intent != ManagerKeep {
				t.Errorf("%s: recover outside keep", where)
			}
			// A create is planned only over no record; a resume never over none.
			if p.verb == verbCreate && c.o.present {
				t.Errorf("%s: create over a present record", where)
			}
			if !c.o.present && (p.verb == verbResume || p.verb == verbResumeForce || p.verb == verbResumeTask) {
				t.Errorf("%s: resume of an absent record", where)
			}
			// Only the token reset reads the hub's container column.
			if col.q.Intent != ManagerTokenReset {
				o := c.o
				o.container = "Up 1 second"
				if q, qerr := planManager(col.q, "m", o); q != p || (qerr == nil) != (err == nil) {
					t.Errorf("%s: the container column changed the plan: %+v/%v, then %+v/%v", where, p, err, q, qerr)
				}
			}
			// podman's word and the guest boot count for keep alone.
			if col.q.Intent != ManagerKeep {
				o := c.o
				o.restarted, o.containerDown, o.recovered = false, false, false
				if q, qerr := planManager(col.q, "m", o); q != p || (qerr == nil) != (err == nil) {
					t.Errorf("%s: podman's or the boot's word changed the plan", where)
				}
			}
		}
	}
}
