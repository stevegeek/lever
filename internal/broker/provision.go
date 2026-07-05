package broker

import (
	"encoding/json"
	"net/http"

	"github.com/lever-to/lever/internal/cap/ca"
)

// ProvisionRequest is the body of POST /provision (manager only).
type ProvisionRequest struct {
	Grove string `json:"grove"`
}

// ProvisionResponse carries the one-time enrolment ticket.
type ProvisionResponse struct {
	Ticket string `json:"ticket"`
}

// handleProvision issues a single-use enrolment ticket for a grove. Only the
// configured manager identity may call it, and only for a configured grove. No
// key material is returned (the grove self-generates its keypair and enrols).
//
// gating: caller == manager && grove ∈ configured agents.
// Possible future refinement: make provisioning itself a rules-governed
// delegated capability, so "the manager is just an agent with a wider policy"
// holds for spawning too (rather than a special-cased manager identity here).
func (b *Broker) handleProvision(w http.ResponseWriter, r *http.Request) {
	caller, err := ca.RequireAgent(r)
	if err != nil {
		b.audit("provision", "", "deny", err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if caller != b.manager {
		b.audit("provision", caller, "deny", "not the manager identity")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// A revoked manager cannot issue new enrolment tickets (spawning fresh
	// agents is a steering channel — see requireManagerGrove).
	if b.isRevoked(caller) {
		b.audit("provision", caller, "deny", "revoked")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req ProvisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		b.audit("provision", caller, "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !b.isAgent(req.Grove) {
		b.audit("provision", caller, "deny", "unknown grove: "+req.Grove)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	tk, err := b.tickets.Issue(req.Grove, b.ticketTTL)
	if err != nil {
		b.audit("provision", caller, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ProvisionResponse{Ticket: tk})
	b.audit("provision", caller, "allow", req.Grove)
}
