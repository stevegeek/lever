package broker

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

// The operator's worker verbs below are on the 0600 operator socket only, as
// handleOperatorWorkerResume is: the socket's file permissions are the gate,
// and they grant no manager or worker anything. Each runs the manager's verb
// with the operator as the actor, so the worker's lock, the role, workspace
// and shared-folder checks, the running-worker cap and the recycle rate limit
// are the same for both callers.

// WorkerActivitySource marks WorkerListEntry.Activity as the worker's own
// word: scion's harness hooks inside the worker's container report it.
const WorkerActivitySource = "worker-reported"

// PhaseNone is WorkerListEntry.Phase for a declared worker the hub has no
// record of.
const PhaseNone = "none"

// WorkerListEntry is one declared worker in `lever worker list`. Every string
// is a known label (scion.PhaseLabel and friends), never hub text: phase,
// activity and container status are agent-settable.
type WorkerListEntry struct {
	Name           string     `json:"name"`
	Phase          string     `json:"phase"`
	Activity       string     `json:"activity"`
	ActivitySource string     `json:"activity_source"`
	LastActivity   *time.Time `json:"last_activity"`
	Container      string     `json:"container"`
	// IdleAtPrompt is nil when the evidence does not settle it. It derives
	// from Activity, so it is worker-reported too.
	IdleAtPrompt *bool `json:"idle_at_prompt"`
}

// WorkerListResponse is the answer of GET PathOperatorWorkerList.
type WorkerListResponse struct {
	Workers []WorkerListEntry `json:"workers"`
}

func (b *Broker) handleOperatorWorkerList(w http.ResponseWriter, r *http.Request) {
	if !b.runtimeReady(w) {
		return
	}
	agents, err := b.runtime.List(r.Context(), b.instanceProject)
	if err != nil {
		b.audit("worker", operatorActor, "error", "list: "+err.Error())
		refuse(w, ReasonRuntimeError, "runtime error", http.StatusBadGateway)
		return
	}
	bySlug := make(map[string]scion.Agent, len(agents))
	for _, a := range agents {
		bySlug[a.Slug] = a
	}
	names := make([]string, 0, len(b.workers))
	for name := range b.workers {
		names = append(names, name)
	}
	slices.Sort(names)
	out := WorkerListResponse{Workers: make([]WorkerListEntry, 0, len(names))}
	for _, name := range names {
		out.Workers = append(out.Workers, workerListEntry(name, bySlug[name], bySlug[name].Slug == name))
	}
	writeJSON(w, out)
}

func workerListEntry(name string, a scion.Agent, found bool) WorkerListEntry {
	e := WorkerListEntry{Name: name, Phase: PhaseNone, ActivitySource: WorkerActivitySource}
	if !found {
		e.IdleAtPrompt = new(bool)
		return e
	}
	e.Phase = scion.PhaseLabel(a.Phase)
	if scion.RunningContainerDown(e.Phase, a.ContainerStatus) {
		e.Phase = PhaseRunningContainerDownToken
	}
	e.Activity = scion.ActivityLabel(a.Activity)
	e.Container = scion.ContainerLabel(a.ContainerStatus)
	if !a.LastActivityEvent.IsZero() {
		t := a.LastActivityEvent.UTC()
		e.LastActivity = &t
	}
	e.IdleAtPrompt = idleAtPrompt(e.Phase, e.Activity)
	return e
}

// idleAtPrompt settles only what the record shows plainly. A worker that is
// not running has no prompt. stalled is set by the hub's sweeper both for a
// harness idle at its prompt and for one that cannot reach the model, so it
// and every other activity stay unknown.
func idleAtPrompt(phase, activity string) *bool {
	v := func(b bool) *bool { return &b }
	if phase != scion.PhaseRunning {
		return v(false)
	}
	switch activity {
	case scion.ActivityWaitingForInput, scion.ActivityCompleted:
		return v(true)
	case scion.ActivityWorking, "thinking", "executing":
		return v(false)
	}
	return nil
}

// operatorWorker decodes an operator worker request into req and resolves
// its worker. The manager is not a worker here: `lever up` owns it.
func (b *Broker) operatorWorker(w http.ResponseWriter, r *http.Request, verb string, req any, name func() string) (WorkerSpec, bool) {
	if err := decodeBody(w, r, jailBodyLimit, req); err != nil {
		b.audit("worker", operatorActor, "deny", verb+": bad body")
		refuse(w, ReasonBadRequest, "bad request", http.StatusBadRequest)
		return WorkerSpec{}, false
	}
	spec, ok := b.workerSpec(name())
	if !ok {
		b.audit("worker", operatorActor, "deny", verb+": not a declared worker")
		refuse(w, ReasonNotAWorker, "not a declared worker (the manager is started with `lever up`)", http.StatusForbidden)
		return WorkerSpec{}, false
	}
	return spec, true
}

// handleOperatorWorkerStart is `lever worker start NAME`: the manager's start
// verb. With no task in the request, a fresh start takes the worker's
// task_file.
func (b *Broker) handleOperatorWorkerStart(w http.ResponseWriter, r *http.Request) {
	var req wire.WorkerStartRequest
	spec, ok := b.operatorWorker(w, r, "start", &req, func() string { return req.Worker })
	if !ok {
		return
	}
	if b.isRevoked(spec.Name) {
		b.audit("worker", operatorActor, "deny", "start "+spec.Name+": revoked")
		refuse(w, ReasonRevoked, "worker "+spec.Name+" is revoked", http.StatusForbidden)
		return
	}
	if !b.runtimeReady(w) {
		return
	}
	b.startWorker(r.Context(), w, spec, req.Task, true, operatorActor)
}

// handleOperatorWorkerRecycle is `lever worker recycle NAME`: the manager's
// recycle verb, for a worker the config marks recyclable. With no task in the
// request, the fresh start takes the worker's task_file.
func (b *Broker) handleOperatorWorkerRecycle(w http.ResponseWriter, r *http.Request) {
	var req wire.WorkerStartRequest
	spec, ok := b.operatorWorker(w, r, "recycle", &req, func() string { return req.Worker })
	if !ok {
		return
	}
	if b.isRevoked(spec.Name) {
		b.audit("worker", operatorActor, "deny", "recycle "+spec.Name+": revoked")
		refuse(w, ReasonRevoked, "worker "+spec.Name+" is revoked; nothing was deleted", http.StatusForbidden)
		return
	}
	if !b.runtimeReady(w) {
		return
	}
	b.recycleWorker(r.Context(), w, spec, req.Task, true, operatorActor)
}

func (b *Broker) handleOperatorWorkerSuspend(w http.ResponseWriter, r *http.Request) {
	b.operatorWorkerHalt(w, r, "suspend", scion.PhaseSuspended, func(ctx context.Context, name string) error {
		return b.runtime.Suspend(ctx, name, b.instanceProject)
	})
}

func (b *Broker) handleOperatorWorkerStop(w http.ResponseWriter, r *http.Request) {
	b.operatorWorkerHalt(w, r, "stop", scion.PhaseStopped, func(ctx context.Context, name string) error {
		return b.runtime.Stop(ctx, name, b.instanceProject)
	})
}

// operatorWorkerHalt is the manager's stop or suspend verb (workerVerb) for
// the operator, under the worker's lock. A worker already in phase target is
// left alone; one with no record is refused, since scion has nothing to act
// on. Neither verb checks revocation: both take authority away.
func (b *Broker) operatorWorkerHalt(w http.ResponseWriter, r *http.Request, verb, target string, do func(context.Context, string) error) {
	var req wire.WorkerRequest
	spec, ok := b.operatorWorker(w, r, verb, &req, func() string { return req.Worker })
	if !ok {
		return
	}
	if !b.runtimeReady(w) {
		return
	}
	ctx := r.Context()
	unlock, ok := b.lockWorkerOrRefuse(w, ctx, operatorActor, verb, spec.Name)
	if !ok {
		return
	}
	defer unlock()
	phase, err := b.phaseOf(ctx, spec)
	if err != nil {
		b.audit("worker", operatorActor, "error", verb+" "+spec.Name+": phase: "+err.Error())
		refuse(w, ReasonRuntimeError, "runtime error", http.StatusBadGateway)
		return
	}
	switch phase {
	case "":
		b.audit("worker", operatorActor, "deny", verb+" "+spec.Name+": no record")
		refuse(w, ReasonNoRecord, "worker "+spec.Name+" has no record on the hub (never started, purged, or a new jail); there is nothing to "+verb, http.StatusNotFound)
		return
	case target:
		b.audit("worker", operatorActor, "allow", verb+" "+spec.Name+": already "+target)
		writeJSON(w, wire.WorkerResponse{Worker: spec.Name, Phase: target})
		return
	}
	if err := do(ctx, spec.Name); err != nil {
		// The detail stays in the audit log: scion's text can echo the
		// container env.
		b.audit("worker", operatorActor, "error", verb+" "+spec.Name+" (phase "+phase+"): "+err.Error())
		refusePhase(w, ReasonRuntimeError, phase, "runtime error: worker "+spec.Name+" (phase "+phase+") did not "+verb+"; see the broker log", http.StatusBadGateway)
		return
	}
	after, err := b.phaseOf(ctx, spec)
	if err != nil || after == "" {
		after = "unknown"
	}
	b.audit("worker", operatorActor, "allow", verb+" "+spec.Name)
	writeJSON(w, wire.WorkerResponse{Worker: spec.Name, Phase: after})
}
