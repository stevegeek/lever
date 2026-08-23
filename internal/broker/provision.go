package broker

import (
	"encoding/json"
	"net/http"

	"github.com/stevegeek/lever/internal/wire"
)

// handleProvision issues a single-use enrolment ticket for a worker. Only the
// configured manager identity may call it, and only for a configured worker. No
// key material is returned (the worker self-generates its keypair and enrols).
//
// gating: caller == manager && worker ∈ configured agents.
// Possible future refinement: make provisioning itself a rules-governed
// delegated capability, so "the manager is just an agent with a wider policy"
// holds for spawning too (rather than a special-cased manager identity here).
func (b *Broker) handleProvision(w http.ResponseWriter, r *http.Request) {
	// A revoked manager cannot issue new enrolment tickets (spawning fresh
	// agents is a steering channel — see requireManagerWorker).
	caller, ok := b.requireManager(w, r, "provision", "")
	if !ok {
		return
	}
	var req wire.ProvisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		b.audit("provision", caller, "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !b.isAgent(req.Worker) {
		b.audit("provision", caller, "deny", "unknown worker: "+req.Worker)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	tk, err := b.tickets.Issue(req.Worker, b.ticketTTL)
	if err != nil {
		b.audit("provision", caller, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, wire.ProvisionResponse{Ticket: tk})
	b.audit("provision", caller, "allow", req.Worker)
}
