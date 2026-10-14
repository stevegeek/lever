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
	verb := "recycle " + spec.Name
	// Config validation already excludes this; checked again because a
	// recycle deletes a record.
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
		// Phase and container status are agent-settable text, so also check
		// the container itself, which no agent can fake.
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
	// Read now so an unreadable file fails with the record still in place.
	if _, err := readWorkerInstructions(spec); err != nil {
		b.audit("worker", b.manager, "error", verb+": "+err.Error())
		http.Error(w, "instructions error; nothing was deleted", http.StatusInternalServerError)
		return
	}
	// Before the purge. The start below admits again under the same name,
	// which is allowed (a worker never counts against itself).
	release, ok := b.admitOrRefuse(ctx, w, b.manager, "recycle", spec.Name)
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
		b.audit("worker", b.manager, "deny", verb+": rate limited")
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		http.Error(w, fmt.Sprintf("rate limited: worker %s was recycled less than a minute ago; try again in %ds. Nothing was deleted.", spec.Name, secs), http.StatusTooManyRequests)
		return
	}
	task := fmt.Sprintf("task %d bytes, begins %q", len(req.Task), taskPreview(req.Task))
	// The fresh start would be refused; refuse before the purge.
	if err := b.checkSharedAccess(ctx, spec, false); err != nil {
		b.recycleRate.refund(spec.Name, counted)
		b.audit("worker", b.manager, "deny", verb+": "+err.Error())
		http.Error(w, "forbidden: "+sharedAccessHint(spec.Name, err)+". Nothing was deleted.", http.StatusForbidden)
		return
	}
	if phase == "" {
		b.audit("worker", b.manager, "allow", verb+": no record, starting fresh; "+task)
	} else {
		ticketErr, err := b.purger.PurgeWorker(ctx, spec.Name)
		if err != nil {
			b.recycleRate.refund(spec.Name, counted)
			b.audit("worker", b.manager, "error", verb+": purge (phase "+phase+"): "+err.Error())
			http.Error(w, "runtime error: the record of worker "+spec.Name+" was not deleted, or its delete was not confirmed; nothing was started. Check `lever-manager agent list` and try again. "+
				"If it persists, the operator should run `lever doctor`.", http.StatusBadGateway)
			return
		}
		if ticketErr != nil {
			// The start stages a fresh ticket over what is left.
			b.audit("worker", b.manager, "error", verb+": removing the staged ticket: "+ticketErr.Error())
		}
		b.audit("worker", b.manager, "allow", verb+": record deleted (phase "+phase+"), workspace kept; "+task)
	}
	b.startFreshWorker(w, r, spec, req.Task)
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
