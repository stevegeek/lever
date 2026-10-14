package broker

import (
	"net/http"

	"github.com/stevegeek/lever/internal/wire"
)

// handleWorkerTicket stages a worker ticket in the guest, as a dispatch does,
// for enrolling a worker identity without a container. Admin listener only:
// it must never be reachable by the manager, whom worker tickets are kept
// from. The ticket value never leaves the broker; the answer is its path.
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
