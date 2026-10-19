package apply

import (
	"context"
	"fmt"
	"testing"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/testutil"
)

// startManagerOutcome is what start-manager did to the record, and whether
// the step failed.
type startManagerOutcome struct {
	verbs string
	fails bool
}

func runStartManagerOver(t *testing.T, o testutil.ManagerObservation, fresh bool) startManagerOutcome {
	t.Helper()
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: o.Phase, initContainerStatus: o.Container}
	deps := fillDeps(Deps{Scion: scion.New(r, scion.Options{}), Log: func(string, ...any) {}})
	err := Run(context.Background(), app, deps, PlanOpts{Fresh: fresh})
	return startManagerOutcome{
		verbs: fmt.Sprintf("create=%d resume=%d force=%d delete=%d", r.startCalls, r.resumeCalls, r.resumeForceCalls, r.deleteCalls),
		fails: err != nil,
	}
}

// TestStartManagerDecisions pins what start-manager does today over the
// shared observation set (characterization: a change here is a behaviour
// change, to make on purpose). A record in a transitional phase never settles
// in this table, so it shows the refusal after the settle wait.
func TestStartManagerDecisions(t *testing.T) {
	const (
		none   = "create=0 resume=0 force=0 delete=0"
		create = "create=1 resume=0 force=0 delete=0"
		resume = "create=0 resume=1 force=0 delete=0"
		force  = "create=0 resume=1 force=1 delete=0"
		recrt  = "create=1 resume=0 force=0 delete=1"
	)
	want := map[string]struct{ keep, fresh startManagerOutcome }{
		"absent":            {startManagerOutcome{create, false}, startManagerOutcome{create, false}},
		"running":           {startManagerOutcome{none, false}, startManagerOutcome{recrt, false}},
		"running-exited":    {startManagerOutcome{none, true}, startManagerOutcome{recrt, false}},
		"running-created":   {startManagerOutcome{none, true}, startManagerOutcome{recrt, false}},
		"running-no-status": {startManagerOutcome{none, true}, startManagerOutcome{recrt, false}},
		"suspended":         {startManagerOutcome{resume, false}, startManagerOutcome{recrt, false}},
		"stopped":           {startManagerOutcome{resume, false}, startManagerOutcome{recrt, false}},
		"error":             {startManagerOutcome{force, false}, startManagerOutcome{recrt, false}},
		"created":           {startManagerOutcome{none, true}, startManagerOutcome{recrt, false}},
		"provisioning":      {startManagerOutcome{none, true}, startManagerOutcome{recrt, false}},
		"cloning":           {startManagerOutcome{none, true}, startManagerOutcome{recrt, false}},
		"starting":          {startManagerOutcome{none, true}, startManagerOutcome{recrt, false}},
		"stopping":          {startManagerOutcome{none, true}, startManagerOutcome{recrt, false}},
		"resumed":           {startManagerOutcome{none, true}, startManagerOutcome{recrt, false}},
		"unknown":           {startManagerOutcome{none, true}, startManagerOutcome{recrt, false}},
	}
	for _, o := range testutil.ManagerObservations {
		w, ok := want[o.Name]
		if !ok {
			t.Errorf("%s: the observation set grew; pin start-manager's decision for it", o.Name)
			continue
		}
		t.Run(o.Name, func(t *testing.T) {
			if got := runStartManagerOver(t, o, false); got != w.keep {
				t.Errorf("apply: got %+v, want %+v", got, w.keep)
			}
			if got := runStartManagerOver(t, o, true); got != w.fresh {
				t.Errorf("--fresh: got %+v, want %+v", got, w.fresh)
			}
		})
	}
}
