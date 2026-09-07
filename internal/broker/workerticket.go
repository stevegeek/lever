package broker

import (
	"net/http"

	"github.com/stevegeek/lever/internal/wire"
)

// handleWorkerTicket mints a one-use enrolment ticket for a declared worker
// and stages it in the guest through the same channel a dispatch uses —
// the host-side mint the acceptance harness needs to enrol a worker
// identity without a container. Admin/loopback only. The ticket value never
// leaves the broker: the response names the staged file's guest path.
//
// This replaced the jail listener's /provision route, which handed a ticket
// for any declared worker to the MANAGER — the identity the channel exists
// to keep worker tickets away from.
func (b *Broker) handleWorkerTicket(w http.ResponseWriter, r *http.Request) {
	var req wire.WorkerTicketRequest
	if err := decodeBody(w, r, smallBodyLimit, &req); err != nil || req.Worker == "" {
		b.audit("worker-ticket", "", "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	spec, ok := b.workerSpec(req.Worker)
	if !ok {
		b.audit("worker-ticket", "", "deny", "unknown worker: "+req.Worker)
		http.Error(w, "unknown worker", http.StatusNotFound)
		return
	}
	if err := b.stageWorkerTicket(r.Context(), spec); err != nil {
		b.audit("worker-ticket", spec.Name, "error", err.Error())
		http.Error(w, "stage error", http.StatusInternalServerError)
		return
	}
	b.audit("worker-ticket", spec.Name, "allow", "")
	writeJSON(w, wire.WorkerTicketResponse{Worker: spec.Name, Path: spec.TicketDir + "/bootstrap.json"})
}
