package broker

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

// WorkerPurger deletes a worker's scion record, then its staged guest ticket;
// never its workspace. err means nothing was done; ticketErr is a ticket
// removal that failed after the record was gone.
type WorkerPurger interface {
	PurgeWorker(ctx context.Context, worker string) (ticketErr, err error)
}

// recycleRateLimit is per worker per minute: far above real use, and it keeps
// a looping manager from churning hub records.
const recycleRateLimit = 1

// taskPreviewLen bounds the task text in an audit line: never the whole task.
const taskPreviewLen = 60

// recyclablePhase reports whether a recycle may delete a record in phase (its
// container must also be down). Error is allowed: its container is dead, so
// no live session is lost. Every other phase is refused, including
// "running, container down", which can still come back.
func recyclablePhase(phase string) bool {
	switch phase {
	case scion.PhaseSuspended, scion.PhaseStopped, scion.PhaseError:
		return true
	}
	return false
}

// handleWorkerRecycle discards a recyclable worker's record and starts it
// fresh with a new task; the workspace is kept. Every guard runs before the
// record is deleted, and the lock is never waited for: a recycle never queues
// behind another lifecycle action.
func (b *Broker) handleWorkerRecycle(w http.ResponseWriter, r *http.Request) {
	var req wire.WorkerStartRequest
	spec, ok := b.requireManagerWorker(w, r, &req, func() string { return req.Worker })
	if !ok {
		return
	}
	b.recycleWorker(r.Context(), w, spec, req.Task, false, b.manager)
}

// recycleWorker is the recycle verb after its caller is authorized: the
// manager's /worker/recycle and the operator's `lever worker recycle` share
// it, and with it the worker's lock and its rate limit. configTask lets an
// empty task take the worker's task_file; only the operator's verb sets it.
func (b *Broker) recycleWorker(ctx context.Context, w http.ResponseWriter, spec WorkerSpec, task string, configTask bool, actor string) {
	verb := "recycle " + spec.Name
	op := actor == operatorActor
	// Config validation already excludes this; checked again because a
	// recycle deletes a record.
	if spec.Name == b.manager || spec.Name == b.managerSlug {
		b.audit("worker", actor, "deny", verb+": the manager is never recycled")
		refuse(w, ReasonNotAWorker, "forbidden", http.StatusForbidden)
		return
	}
	// recyclable is the manager's authority to discard a record. The
	// operator owns the instance and may discard any worker's record anyway
	// (`lever worker purge`), so the operator's recycle does not need it.
	if !op && !spec.Recyclable {
		b.audit("worker", actor, "deny", verb+": not recyclable")
		refuse(w, ReasonNotRecyclable, "worker "+spec.Name+" is not recyclable: the operator must set `recyclable: true` on it under `workers:` in lever.yaml "+
			"(then `lever reload`). Until then, ask the operator to run `lever worker purge "+spec.Name+" --force`.", http.StatusForbidden)
		return
	}
	if !b.taskOK(w, actor, verb, task, "") {
		return
	}
	if b.purger == nil {
		b.audit("worker", actor, "error", verb+": no purge channel is wired (DispatchConfig.Purge)")
		refuse(w, ReasonRuntimeUnavailable, "worker recycle unavailable", http.StatusBadGateway)
		return
	}
	unlock, ok := b.tryLockWorker(spec.Name)
	if !ok {
		b.audit("worker", actor, "deny", verb+": busy (another lifecycle operation holds the worker)")
		refuse(w, ReasonBusy, "busy: another start, resume, stop or heal of "+spec.Name+" is under way; nothing was deleted. Try again shortly.", http.StatusServiceUnavailable)
		return
	}
	defer unlock()
	if op && b.isRevoked(spec.Name) {
		// The fresh start would stage a revoked identity a ticket.
		b.audit("worker", actor, "deny", verb+": revoked")
		refuse(w, ReasonRevoked, "worker "+spec.Name+" is revoked; nothing was deleted", http.StatusForbidden)
		return
	}
	phase, containerStatus, err := b.recordOf(ctx, spec)
	if err != nil {
		b.audit("worker", actor, "error", verb+": phase: "+err.Error())
		refuse(w, ReasonRuntimeError, "runtime error", http.StatusBadGateway)
		return
	}
	notStopped := func(why string) {
		b.audit("worker", actor, "deny", verb+": not stopped ("+why+")")
		first := "Run `lever-manager agent stop " + spec.Name + "` (or `suspend`), then recycle."
		if op {
			first = "Run `lever worker stop " + spec.Name + "` (or `suspend`), then recycle."
		}
		refusePhase(w, ReasonNotStopped, phase, "worker "+spec.Name+" is "+why+"; a recycle deletes only a suspended, stopped or error record whose container is down, never a live session. "+
			first+" Nothing was deleted.", http.StatusConflict)
	}
	if phase != "" && !recyclablePhase(phase) {
		notStopped("phase " + phase)
		return
	}
	if phase != "" {
		// Phase and container status are agent-settable text, so also check
		// the container itself, which no agent can fake.
		if scion.ContainerLive(containerStatus) {
			notStopped("phase " + phase + " but its container is live")
			return
		}
		if b.containerRunning != nil {
			live, err := b.containerRunning(ctx, spec.Name)
			if err != nil {
				b.audit("worker", actor, "error", verb+": container state: "+err.Error())
				refuse(w, ReasonRuntimeError, "runtime error: could not read the state of worker "+spec.Name+"'s container; nothing was deleted", http.StatusBadGateway)
				return
			}
			if live {
				notStopped("phase " + phase + " but its container is running")
				return
			}
		}
	}
	// Read now so an unreadable file fails with the record still in place.
	if _, err := readWorkerInstructions(spec); err != nil {
		b.audit("worker", actor, "error", verb+": "+err.Error())
		refuse(w, ReasonConfigFile, "instructions error; nothing was deleted", http.StatusInternalServerError)
		return
	}
	if configTask && strings.TrimSpace(task) == "" {
		if task, err = readWorkerTask(spec); err != nil {
			b.audit("worker", actor, "error", verb+": "+err.Error())
			refuse(w, ReasonConfigFile, "task_file error: "+err.Error()+"; nothing was deleted", http.StatusInternalServerError)
			return
		}
	}
	// Before the purge. The start below admits again under the same name,
	// which is allowed (a worker never counts against itself).
	release, ok := b.admitOrRefuse(ctx, w, actor, "recycle", spec.Name)
	if !ok {
		return
	}
	defer release()
	// Counted last, so a refusal above does not use up the worker's slot.
	counted := time.Now()
	if ok, wait := b.recycleRate.take(spec.Name, counted); !ok {
		secs := int(wait.Round(time.Second) / time.Second)
		if secs < 1 {
			secs = 1
		}
		b.audit("worker", actor, "deny", verb+": rate limited")
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		refuse(w, ReasonRateLimited, fmt.Sprintf("rate limited: worker %s was recycled less than a minute ago; try again in %ds. Nothing was deleted.", spec.Name, secs), http.StatusTooManyRequests)
		return
	}
	preview := fmt.Sprintf("task %d bytes, begins %q", len(task), taskPreview(task))
	// The fresh start would be refused; refuse before the purge.
	if err := b.checkSharedAccess(ctx, spec, false); err != nil {
		b.recycleRate.refund(spec.Name, counted)
		b.audit("worker", actor, "deny", verb+": "+err.Error())
		refuse(w, ReasonSharedAccess, "forbidden: "+sharedAccessHint(spec.Name, err)+". Nothing was deleted.", http.StatusForbidden)
		return
	}
	if phase == "" {
		b.audit("worker", actor, "allow", verb+": no record, starting fresh; "+preview)
	} else {
		ticketErr, err := b.purger.PurgeWorker(ctx, spec.Name)
		if err != nil {
			b.recycleRate.refund(spec.Name, counted)
			b.audit("worker", actor, "error", verb+": purge (phase "+phase+"): "+err.Error())
			check := "Check `lever-manager agent list` and try again. If it persists, the operator should run `lever doctor`."
			if op {
				check = "Check `lever worker list` and try again. If it persists, run `lever doctor`."
			}
			refuse(w, ReasonRuntimeError, "runtime error: the record of worker "+spec.Name+" was not deleted, or its delete was not confirmed; nothing was started. "+check, http.StatusBadGateway)
			return
		}
		if ticketErr != nil {
			// The start stages a fresh ticket over what is left.
			b.audit("worker", actor, "error", verb+": removing the staged ticket: "+ticketErr.Error())
		}
		b.audit("worker", actor, "allow", verb+": record deleted (phase "+phase+"), workspace kept; "+preview)
	}
	b.startFreshWorker(ctx, w, spec, task, false, actor)
}

func taskPreview(task string) string {
	s := strings.ToValidUTF8(task, "_")
	if len(s) > taskPreviewLen {
		s = strings.ToValidUTF8(s[:taskPreviewLen], "")
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
}
