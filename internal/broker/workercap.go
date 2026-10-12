package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/scion"
)

// broker.max_running_workers (lever#193): the broker refuses to bring up a
// worker while the count of OTHER workers that are running or on their way
// up has reached the cap. The cheapest way to keep a small machine from
// thrashing: a start the VM has no memory for fails at once, by name, rather
// than as an OOM kill of whichever process is largest (often the manager).
//
// Every path that adds a running worker is admitted here: the start route
// (fresh or a resume of a record), the resume verb, the remote chat page's
// wake, recycle (before its purge, so a refusal deletes nothing), and the
// re-enrol healer's resume of a record that is not running. A healer bounce
// of a running worker (suspend+resume) adds none and is not counted.
//
// What counts is read from the hub's records: every agent in the instance
// project except the manager whose phase is not suspended, stopped or error,
// or whose container status reads live. A phase lever does not recognise
// counts (fail closed). Phase and container status are text an agent can
// set about itself, so a worker that lies about both can slip out of the
// count: the cap is a resource guard against an over-eager manager, not a
// containment boundary (each container's own resources limit is that).
//
// Starts already admitted but not yet visible in the records (between the
// admission and scion's create) are held in admitting, so two starts at once
// cannot both take the last slot.

// ErrWorkerCap is the refusal when the cap is reached.
var ErrWorkerCap = errors.New("broker.max_running_workers reached")

// workerCapError names the cap and the workers that hold it.
type workerCapError struct {
	worker string
	max    int
	active []string // declared workers that count, sorted
	others int      // agents that count but are not declared workers
}

func (e *workerCapError) Error() string {
	who := strings.Join(e.active, ", ")
	if e.others > 0 {
		if who != "" {
			who += " and "
		}
		who += fmt.Sprintf("%d other agent(s)", e.others)
	}
	return fmt.Sprintf("worker %s was not started: broker.max_running_workers is %d and %d worker(s) are running or starting (%s). "+
		"Suspend or stop one first (`lever-manager agent suspend NAME`), or ask the operator to raise broker.max_running_workers. Nothing was changed.",
		e.worker, e.max, len(e.active)+e.others, who)
}

func (e *workerCapError) Unwrap() error { return ErrWorkerCap }

// countsAsRunning reports whether a record holds a worker slot.
func countsAsRunning(phase, containerStatus string) bool {
	if scion.ContainerLive(containerStatus) {
		return true
	}
	switch scion.PhaseLabel(phase) {
	case "", scion.PhaseSuspended, scion.PhaseStopped, scion.PhaseError:
		return false
	}
	return true
}

// admitWorker takes a running-worker slot for name, or refuses with a
// *workerCapError (errors.Is ErrWorkerCap). The caller holds name's
// lifecycle lock and calls release when its start or resume has returned,
// whatever the outcome: by then a started worker shows in the records. With
// no cap configured it reads nothing and always admits. A failed read of
// the records is an error (fail closed).
func (b *Broker) admitWorker(ctx context.Context, name string) (release func(), err error) {
	if b.maxRunningWorkers <= 0 {
		return func() {}, nil
	}
	agents, err := b.runtime.List(ctx, b.instanceProject)
	if err != nil {
		return nil, fmt.Errorf("reading the running workers for broker.max_running_workers: %w", err)
	}
	counted := map[string]bool{}
	for _, a := range agents {
		if a.Slug == name || a.Slug == b.managerSlug || a.Slug == b.manager {
			continue
		}
		if countsAsRunning(a.Phase, a.ContainerStatus) {
			counted[a.Slug] = true
		}
	}
	b.admitMu.Lock()
	defer b.admitMu.Unlock()
	for other := range b.admitting {
		if other != name {
			counted[other] = true
		}
	}
	if len(counted) >= b.maxRunningWorkers {
		e := &workerCapError{worker: name, max: b.maxRunningWorkers}
		for slug := range counted {
			if _, declared := b.workers[slug]; declared {
				e.active = append(e.active, slug)
			} else {
				e.others++
			}
		}
		slices.Sort(e.active)
		return nil, e
	}
	if b.admitting == nil {
		b.admitting = map[string]int{}
	}
	b.admitting[name]++
	return func() {
		b.admitMu.Lock()
		defer b.admitMu.Unlock()
		if b.admitting[name]--; b.admitting[name] <= 0 {
			delete(b.admitting, name)
		}
	}, nil
}

// admitOrRefuse is admitWorker for a handler: a refusal answers 409 with the
// cap's message, a failed read 502; both are audited under actor.
func (b *Broker) admitOrRefuse(ctx context.Context, w http.ResponseWriter, actor, verb, name string) (func(), bool) {
	release, err := b.admitWorker(ctx, name)
	if err == nil {
		return release, true
	}
	if errors.Is(err, ErrWorkerCap) {
		b.audit("worker", actor, "deny", verb+" "+name+": "+err.Error())
		http.Error(w, err.Error(), http.StatusConflict)
		return nil, false
	}
	b.audit("worker", actor, "error", verb+" "+name+": "+err.Error())
	http.Error(w, "runtime error", http.StatusBadGateway)
	return nil, false
}
