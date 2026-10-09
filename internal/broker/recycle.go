package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

// WorkerPurger deletes a worker's scion record and then its staged guest
// ticket: the teardown of `lever worker purge` (brokerctl.PurgeWorker).
// err is the record delete's failure, with nothing done; ticketErr a ticket
// removal that failed after the record was gone. It never touches the
// worker's workspace.
type WorkerPurger interface {
	PurgeWorker(ctx context.Context, worker string) (ticketErr, err error)
}

// recycleRateLimit is how many recycles of ONE worker the broker allows in a
// minute. A deal slot is recycled when its deal ends, so one a minute is far
// above real use, and it keeps a manager in a loop from churning hub records.
const recycleRateLimit = 1

// taskPreviewLen bounds how much of a recycle's task goes into the audit
// line: enough to tell two deals apart, never the whole task.
const taskPreviewLen = 60

// recyclablePhase reports whether a worker record in phase may be deleted by
// a recycle (its container must also be down: handleWorkerRecycle). Suspended and stopped are the phases the manager's own verbs
// leave. Error is allowed too: the hub sets it for a record whose container
// is dead (a crash, or a worker left running across `lever stop` + `lever
// up`), so no live session is lost, and a purge plus a fresh start is the
// documented recovery for such a record. Every other phase is refused:
// running, a record the hub still calls running while its container is down
// (it can still come back; stop it first), the interim phases of a start,
// resume or stop under way, and an unrecognised phase.
func recyclablePhase(phase string) bool {
	switch phase {
	case scion.PhaseSuspended, scion.PhaseStopped, scion.PhaseError:
		return true
	}
	return false
}

// handleWorkerRecycle discards a declared worker's record and starts it
// fresh with a new task: the manager's form of `lever worker purge` followed
// by `agent start`, for a worker slot the operator marked recyclable. The
// old conversation is lost; the workspace (work product) is kept.
//
// Guards, in order: the caller is the manager and not revoked, the worker is
// declared (requireManagerWorker); it is not the manager; it is recyclable;
// the task passes the start route's checks; the worker's lifecycle lock is
// free (no waiting: a recycle never queues behind a start, resume, wake or
// heal); the record is in a recyclable phase; the instructions file reads;
// the per-worker rate allows it. Only then is the record deleted, and the
// start that follows is the start route's own (startFreshWorker), under the
// same lock.
func (b *Broker) handleWorkerRecycle(w http.ResponseWriter, r *http.Request) {
	var req wire.WorkerStartRequest
	spec, ok := b.requireManagerWorker(w, r, &req, func() string { return req.Worker })
	if !ok {
		return
	}
	verb := "recycle " + spec.Name
	// Config validation keeps the manager's CN and slug out of workers:, and
	// the lookup above is exact. Checked again here: a recycle deletes a
	// record, and the manager's record holds its conversation.
	if spec.Name == b.manager || spec.Name == b.managerSlug {
		b.audit("worker", b.manager, "deny", verb+": the manager is never recycled")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !spec.Recyclable {
		b.audit("worker", b.manager, "deny", verb+": not recyclable")
		http.Error(w, "worker "+spec.Name+" is not recyclable: the operator must set `recyclable: true` on it under `workers:` in lever.yaml "+
			"(then `lever reload`). Until then, ask the operator to run `lever worker purge "+spec.Name+" --force`.", http.StatusForbidden)
		return
	}
	if err := scion.CheckTask(req.Task); err != nil {
		b.audit("worker", b.manager, "deny", verb+": "+err.Error())
		status := http.StatusBadRequest
		var tooLong *scion.TaskTooLongError
		if errors.As(err, &tooLong) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}
	if b.purger == nil {
		b.audit("worker", b.manager, "error", verb+": no purge channel is wired (DispatchConfig.Purge)")
		http.Error(w, "worker recycle unavailable", http.StatusBadGateway)
		return
	}
	unlock, ok := b.tryLockWorker(spec.Name)
	if !ok {
		b.audit("worker", b.manager, "deny", verb+": busy (another lifecycle operation holds the worker)")
		http.Error(w, "busy: another start, resume, stop or heal of "+spec.Name+" is under way; nothing was deleted. Try again shortly.", http.StatusServiceUnavailable)
		return
	}
	defer unlock()
	ctx := r.Context()
	phase, containerStatus, err := b.recordOf(ctx, spec)
	if err != nil {
		b.audit("worker", b.manager, "error", verb+": phase: "+err.Error())
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	notStopped := func(why string) {
		b.audit("worker", b.manager, "deny", verb+": not stopped ("+why+")")
		http.Error(w, "worker "+spec.Name+" is "+why+"; a recycle deletes only a suspended, stopped or error record whose container is down, never a live session. "+
			"Run `lever-manager agent stop "+spec.Name+"` (or `suspend`), then recycle. Nothing was deleted.", http.StatusConflict)
	}
	if phase != "" && !recyclablePhase(phase) {
		notStopped("phase " + phase)
		return
	}
	if phase != "" {
		// The phase and the container status are both text a running worker
		// can post about itself, so a phase alone is not proof the session
		// is down. The hub's container status is checked first, then the
		// container itself, which no agent can fake.
		if scion.ContainerLive(containerStatus) {
			notStopped("phase " + phase + " but its container is live")
			return
		}
		if b.containerRunning != nil {
			live, err := b.containerRunning(ctx, spec.Name)
			if err != nil {
				b.audit("worker", b.manager, "error", verb+": container state: "+err.Error())
				http.Error(w, "runtime error: could not read the state of worker "+spec.Name+"'s container; nothing was deleted", http.StatusBadGateway)
				return
			}
			if live {
				notStopped("phase " + phase + " but its container is running")
				return
			}
		}
	}
	// The start below reads the same file; read it now so a config that names
	// an unreadable file fails with the record still in place.
	if _, err := readWorkerInstructions(spec); err != nil {
		b.audit("worker", b.manager, "error", verb+": "+err.Error())
		http.Error(w, "instructions error; nothing was deleted", http.StatusInternalServerError)
		return
	}
	// Counted last, so a refusal above does not use up the worker's slot.
	if ok, wait := b.recycleRate.take(spec.Name, time.Now()); !ok {
		secs := int(wait.Round(time.Second) / time.Second)
		if secs < 1 {
			secs = 1
		}
		b.audit("worker", b.manager, "deny", verb+": rate limited")
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		http.Error(w, fmt.Sprintf("rate limited: worker %s was recycled less than a minute ago; try again in %ds. Nothing was deleted.", spec.Name, secs), http.StatusTooManyRequests)
		return
	}
	task := fmt.Sprintf("task %d bytes, begins %q", len(req.Task), taskPreview(req.Task))
	// shared_folders: the fresh start would be refused while the manager
	// lacks its pins; refuse before the purge, so nothing is deleted.
	if err := b.checkSharedAccess(ctx, spec, false); err != nil {
		b.recycleRate.refund(spec.Name)
		b.audit("worker", b.manager, "deny", verb+": "+err.Error())
		http.Error(w, "forbidden: "+sharedAccessHint(spec.Name, err)+". Nothing was deleted.", http.StatusForbidden)
		return
	}
	if phase == "" {
		// No record (never started, purged, a new jail): nothing to delete,
		// and the recycle is a fresh start.
		b.audit("worker", b.manager, "allow", verb+": no record, starting fresh; "+task)
	} else {
		ticketErr, err := b.purger.PurgeWorker(ctx, spec.Name)
		if err != nil {
			// Nothing was deleted: the next try may go ahead at once.
			b.recycleRate.refund(spec.Name)
			b.audit("worker", b.manager, "error", verb+": purge (phase "+phase+"): "+err.Error())
			http.Error(w, "runtime error: the record of worker "+spec.Name+" was not deleted; check `lever-manager agent list` and try again. "+
				"If it persists, the operator should run `lever doctor`.", http.StatusBadGateway)
			return
		}
		if ticketErr != nil {
			// The record is gone; the start stages a fresh ticket over what is
			// left, so this is only the operator's record.
			b.audit("worker", b.manager, "error", verb+": removing the staged ticket: "+ticketErr.Error())
		}
		b.audit("worker", b.manager, "allow", verb+": record deleted (phase "+phase+"), workspace kept; "+task)
	}
	b.startFreshWorker(w, r, spec, req.Task)
}

// taskPreview is the start of a task for an audit line: at most
// taskPreviewLen bytes of valid UTF-8, control and format characters
// replaced by spaces.
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
