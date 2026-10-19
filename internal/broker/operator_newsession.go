package broker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

// stopWaitAttempts bounds the wait for a stopped record, at b.liveInterval
// apart: a container's graceful stop can take scion's whole stop timeout.
const stopWaitAttempts = 120

var errStopUnconfirmed = errors.New("the stop was not confirmed")

// handleOperatorWorkerNewSession is `lever worker new-session NAME`: it keeps
// the worker's record and workspace and drops its conversation. The worker
// is stopped, which ends its session, and the stopped record is resumed in a
// new session, as `lever conversation new` does for the manager. Every check
// that can refuse the resume runs before the stop, so a refusal leaves the
// session as it was.
//
// There is no message form of this: a message must never become a slash
// command, so a typed /clear stays impossible.
func (b *Broker) handleOperatorWorkerNewSession(w http.ResponseWriter, r *http.Request) {
	var req wire.WorkerStartRequest
	spec, ok := b.operatorWorker(w, r, "new-session", &req, func() string { return req.Worker })
	if !ok {
		return
	}
	verb := "new-session " + spec.Name
	if b.isRevoked(spec.Name) {
		b.audit("worker", operatorActor, "deny", verb+": revoked")
		refuse(w, ReasonRevoked, "worker "+spec.Name+" is revoked", http.StatusForbidden)
		return
	}
	if !b.runtimeReady(w) {
		return
	}
	if !b.taskOK(w, operatorActor, verb, req.Task, "") {
		return
	}
	ctx := r.Context()
	unlock, ok := b.lockWorkerOrRefuse(w, ctx, operatorActor, "new-session", spec.Name)
	if !ok {
		return
	}
	defer unlock()
	if b.isRevoked(spec.Name) {
		// Under the lock: a revoke that landed while this waited must not
		// get the worker a fresh ticket.
		b.audit("worker", operatorActor, "deny", verb+": revoked")
		refuse(w, ReasonRevoked, "worker "+spec.Name+" is revoked", http.StatusForbidden)
		return
	}
	phase, err := b.phaseOf(ctx, spec)
	if err != nil {
		b.audit("worker", operatorActor, "error", verb+": phase: "+err.Error())
		refuse(w, ReasonRuntimeError, "runtime error", http.StatusBadGateway)
		return
	}
	if phase == "" {
		// scion's resume of a missing record would create the agent from the
		// project template, not the worker's config.
		b.audit("worker", operatorActor, "deny", verb+": no record")
		refuse(w, ReasonNoRecord, "worker "+spec.Name+" has no record on the hub (never started, purged, or a new jail); start it instead: `lever worker start "+spec.Name+"`", http.StatusNotFound)
		return
	}
	task := req.Task
	if strings.TrimSpace(task) == "" {
		if task, err = readWorkerTask(spec); err != nil {
			b.audit("worker", operatorActor, "error", verb+": "+err.Error())
			refuse(w, ReasonConfigFile, "task_file error: "+err.Error()+"; nothing was changed", http.StatusInternalServerError)
			return
		}
	}
	if err := b.checkAgentRole(ctx, spec.Name); err != nil {
		b.audit("worker", operatorActor, "deny", verb+": "+err.Error())
		refuse(w, ReasonRoleMismatch, err.Error(), http.StatusConflict)
		return
	}
	release, ok := b.admitOrRefuse(ctx, w, operatorActor, "new-session", spec.Name)
	if !ok {
		return
	}
	defer release()
	if !b.resumeChecks(ctx, w, spec, operatorActor) {
		return
	}
	// Always asked, whatever the phase reads: the phase is hub text the
	// worker can set, and a suspended record would take the task into the
	// session it continues.
	if err := b.runtime.Stop(ctx, spec.Name, b.instanceProject); err != nil {
		b.audit("worker", operatorActor, "error", verb+": stop (phase "+phase+"): "+err.Error())
		refusePhase(w, ReasonRuntimeError, phase, "runtime error: worker "+spec.Name+" did not stop, so no new session was started; its session is unchanged unless the stop landed. Check `lever worker list` and try again", http.StatusBadGateway)
		return
	}
	if err := b.waitWorkerStopped(ctx, spec); err != nil {
		b.audit("worker", operatorActor, "error", verb+": "+err.Error())
		refuse(w, ReasonRuntimeError, "runtime error: worker "+spec.Name+" did not read stopped with its container down in time, so no new session was started. The worker may be stopped: run `lever worker new-session "+spec.Name+"` again, or `lever worker resume "+spec.Name+"`", http.StatusBadGateway)
		return
	}
	b.audit("worker", operatorActor, "allow", verb+": stopped (was "+phase+"); "+taskAudit(task))
	b.resumeRecordAs(ctx, w, spec, scion.PhaseStopped, operatorActor, &newSession{task: task})
}

// waitWorkerStopped waits until the record reads stopped and its container
// is down, both on the hub and, when wired, in podman, which no agent can
// fake.
func (b *Broker) waitWorkerStopped(ctx context.Context, spec WorkerSpec) error {
	last := "none"
	for range stopWaitAttempts {
		phase, cs, err := b.recordOf(ctx, spec)
		switch {
		case err != nil:
			last = "unreadable"
		case phase == scion.PhaseStopped && !scion.ContainerLive(cs):
			if b.containerRunning == nil {
				return nil
			}
			live, err := b.containerRunning(ctx, spec.Name)
			if err == nil && !live {
				return nil
			}
			last = "stopped, container running"
		default:
			last = cmp.Or(phase, "none")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.liveInterval):
		}
	}
	return fmt.Errorf("%w (last seen: %s)", errStopUnconfirmed, last)
}

// taskAudit names a task in an audit line by size and its start only.
func taskAudit(task string) string {
	if task == "" {
		return "no task (the hub's stored one)"
	}
	return fmt.Sprintf("task %d bytes, begins %q", len(task), taskPreview(task))
}
