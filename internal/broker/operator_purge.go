package broker

import (
	"net/http"
	"regexp"

	"github.com/stevegeek/lever/internal/scion"
)

// PathOperatorWorkerPurge is `lever worker purge`'s route on the 0600
// operator socket.
const PathOperatorWorkerPurge = "/operator/worker/purge"

// ReasonNotOrphan refuses an orphan purge of a name that is a declared
// worker (403): purge it as a worker.
const ReasonNotOrphan = "not-orphan"

// WorkerPurgeRequest is the body of PathOperatorWorkerPurge. Orphan names a
// hub record of the instance's project that is neither the manager nor a
// declared worker.
type WorkerPurgeRequest struct {
	Worker string `json:"worker"`
	Orphan bool   `json:"orphan,omitempty"`
}

// WorkerPurgeResponse answers a purge that deleted the record, or found none
// for a declared worker. Was is the record's phase label before ("none": no
// record). TicketLeft: the staged ticket directory could not be removed (the
// next start stages over it). CutoffUnsaved: the certificate cutoff holds
// until the broker restarts but was not saved.
type WorkerPurgeResponse struct {
	Worker        string `json:"worker"`
	Was           string `json:"was"`
	TicketLeft    bool   `json:"ticket_left,omitempty"`
	CutoffUnsaved bool   `json:"cutoff_unsaved,omitempty"`
}

// purgeNameRE mirrors config.nameRE. An orphan's name is hub text, so it is
// checked before it reaches the audit log or the scion argv.
var purgeNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// IsAgentName reports whether s has the shape of an agent name.
func IsAgentName(s string) bool { return purgeNameRE.MatchString(s) }

// handleOperatorWorkerPurge deletes a worker's scion record and staged ticket
// (never its workspace), under the worker's lock, then supersedes the
// certificates issued to it. Like the purge it replaces, it does not wait for
// the worker to stop: the operator's --force is the confirmation. A declared
// worker with no record is only superseded. An orphan must have a record.
func (b *Broker) handleOperatorWorkerPurge(w http.ResponseWriter, r *http.Request) {
	var req WorkerPurgeRequest
	if err := decodeBody(w, r, jailBodyLimit, &req); err != nil {
		b.audit("worker", operatorActor, "deny", "purge: bad body")
		refuse(w, ReasonBadRequest, "bad request", http.StatusBadRequest)
		return
	}
	name := req.Worker
	if !purgeNameRE.MatchString(name) {
		b.audit("worker", operatorActor, "deny", "purge: not an agent name")
		refuse(w, ReasonBadRequest, "bad request: not an agent name", http.StatusBadRequest)
		return
	}
	verb := "purge " + name
	if req.Orphan {
		verb = "purge orphan " + name
	}
	if name == b.manager || name == b.managerSlug {
		b.audit("worker", operatorActor, "deny", verb+": the manager is never purged")
		refuse(w, ReasonNotAWorker, "the manager is never purged (it is run with `lever up`)", http.StatusForbidden)
		return
	}
	declared := b.workers.has(name)
	switch {
	case req.Orphan && declared:
		b.audit("worker", operatorActor, "deny", verb+": a declared worker")
		refuse(w, ReasonNotOrphan, name+" is a declared worker, not an orphan: purge it without --orphan", http.StatusForbidden)
		return
	case !req.Orphan && !declared:
		b.audit("worker", operatorActor, "deny", verb+": not a declared worker")
		refuse(w, ReasonNotAWorker, "not a declared worker (a hub record that is neither the manager nor a declared worker is purged with --orphan)", http.StatusForbidden)
		return
	}
	if !b.runtimeReady(w) {
		return
	}
	if b.purger == nil {
		b.audit("worker", operatorActor, "error", verb+": no purge channel is wired (DispatchConfig.Purge)")
		refuse(w, ReasonRuntimeUnavailable, "worker purge unavailable", http.StatusBadGateway)
		return
	}
	// Never queued: a purge that waited would delete a record the start it
	// waited for had just made.
	unlock, ok := b.tryLockWorker(name)
	if !ok {
		b.audit("worker", operatorActor, "deny", verb+": busy (another lifecycle operation holds the worker)")
		refuse(w, ReasonBusy, "busy: another start, resume, stop or heal of "+name+" is under way; nothing was deleted. Try again shortly.", http.StatusServiceUnavailable)
		return
	}
	defer unlock()
	ctx := r.Context()
	agents, err := b.runtime.List(ctx, b.instanceProject)
	if err != nil {
		b.audit("worker", operatorActor, "error", verb+": list: "+err.Error())
		refuse(w, ReasonRuntimeError, "runtime error; nothing was deleted", http.StatusBadGateway)
		return
	}
	resp := WorkerPurgeResponse{Worker: name, Was: PhaseNone}
	if rec := scion.FindAgent(agents, name); rec != nil {
		resp.Was = scion.PhaseLabel(rec.Phase)
		ticketErr, err := b.purger.PurgeWorker(ctx, name)
		if err != nil {
			b.audit("worker", operatorActor, "error", verb+": (phase "+resp.Was+"): "+err.Error())
			refuse(w, ReasonRuntimeError, "runtime error: the record of "+name+" was not deleted, or its delete was not confirmed; its certificates were left alone. "+
				"Check `lever worker list`, then purge again.", http.StatusBadGateway)
			return
		}
		if ticketErr != nil {
			resp.TicketLeft = true
			b.audit("worker", operatorActor, "error", verb+": removing the staged ticket: "+ticketErr.Error())
		}
	} else if req.Orphan {
		b.audit("worker", operatorActor, "deny", verb+": no record")
		refuse(w, ReasonNoRecord, "the hub has no record named "+name+"; there is nothing to purge", http.StatusNotFound)
		return
	}
	if err := b.Supersede(name); err != nil {
		resp.CutoffUnsaved = true
		b.audit("worker", operatorActor, "error", verb+": saving the certificate cutoff: "+err.Error())
	}
	b.audit("worker", operatorActor, "allow", verb+": record deleted (was "+resp.Was+"), certificates superseded, workspace kept")
	writeJSON(w, resp)
}
