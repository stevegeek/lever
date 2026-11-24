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

// broker.max_running_workers: a worker is not brought up while the count of
// OTHER running or starting workers has reached the cap, so a start the VM
// has no memory for fails by name instead of as an OOM kill. Every path that
// adds a running worker is admitted here.
//
// The count is read from the hub's records, and an unrecognised phase counts
// (fail closed). Phase and container status are agent-settable, so a lying
// worker can slip out of the count: the cap is a resource guard, not a
// containment boundary (each container's resources limit is that).
//
// Starts admitted but not yet in the records are held in admitting, so two
// starts at once cannot both take the last slot.

// ErrWorkerCap is the refusal when the cap is reached.
var ErrWorkerCap = errors.New("broker.max_running_workers reached")

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
		"Suspend or stop one first (`lever-manager agent suspend NAME`); for a record that never came up or an agent that is not a declared worker, ask the operator to purge it (`lever worker purge NAME --force`, with `--orphan` for an agent that is not a declared worker) or to raise broker.max_running_workers. Nothing was changed.",
		e.worker, e.max, len(e.active)+e.others, who)
}

func (e *workerCapError) Unwrap() error { return ErrWorkerCap }

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

// admitWorker takes a running-worker slot for name. The caller holds name's
// lifecycle lock and calls release once its start or resume has returned,
// whatever the outcome. A failed read of the records refuses (fail closed).
func (b *Broker) admitWorker(ctx context.Context, name string) (release func(), err error) {
	// A spawned agent also counts against its template's max_running,
	// whatever the global cap.
	tmpl, perTemplate := b.workers.templateOfRecord(name)
	if b.maxRunningWorkers <= 0 && !perTemplate {
		return func() {}, nil
	}
	// Records are read under the lock, or a read taken before another
	// admission's start finished would miss that worker once it is released.
	// Under admitMu only the registry's read lock is taken (the template
	// count); nothing holding the registry lock takes admitMu.
	b.admitMu.Lock()
	defer b.admitMu.Unlock()
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
	for other := range b.admitting {
		if other != name {
			counted[other] = true
		}
	}
	if perTemplate {
		var same []string
		for slug := range counted {
			if t, ok := b.workers.templateOfRecord(slug); ok && t.Name == tmpl.Name {
				same = append(same, slug)
			}
		}
		if len(same) >= tmpl.MaxRunning {
			slices.Sort(same)
			return nil, &templateCapError{agent: name, template: tmpl.Name, max: tmpl.MaxRunning, active: same}
		}
	}
	if b.maxRunningWorkers > 0 && len(counted) >= b.maxRunningWorkers {
		e := &workerCapError{worker: name, max: b.maxRunningWorkers}
		for slug := range counted {
			if b.workers.has(slug) {
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

// ErrTemplateCap is the refusal when a template's max_running is reached.
var ErrTemplateCap = errors.New("template max_running reached")

type templateCapError struct {
	agent, template string
	max             int
	active          []string
}

func (e *templateCapError) Error() string {
	return fmt.Sprintf("agent %s was not started: template %s allows %d running at once and %d are running or starting (%s). Suspend or destroy one first.",
		e.agent, e.template, e.max, len(e.active), strings.Join(e.active, ", "))
}

func (e *templateCapError) Unwrap() error { return ErrTemplateCap }

// admitOrRefuse names the running workers to the manager only; any other
// actor gets a short body.
func (b *Broker) admitOrRefuse(ctx context.Context, w http.ResponseWriter, actor, verb, name string) (func(), bool) {
	release, err := b.admitWorker(ctx, name)
	if err == nil {
		return release, true
	}
	if errors.Is(err, ErrTemplateCap) {
		b.audit("worker", actor, "deny", verb+" "+name+": "+err.Error())
		refuse(w, ReasonTemplateBusy, err.Error(), http.StatusConflict)
		return nil, false
	}
	if errors.Is(err, ErrWorkerCap) {
		b.audit("worker", actor, "deny", verb+" "+name+": "+err.Error())
		msg := err.Error()
		if actor != b.manager {
			msg = "busy: too many workers are running"
		}
		refuse(w, ReasonWorkerCap, msg, http.StatusConflict)
		return nil, false
	}
	b.audit("worker", actor, "error", verb+" "+name+": "+err.Error())
	refuse(w, ReasonRuntimeError, "runtime error", http.StatusBadGateway)
	return nil, false
}
