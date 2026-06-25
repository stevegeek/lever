package broker

import (
	"encoding/json"
	"net/http"
)

// handleBootstrap mints the manager's single-use enrolment ticket. Loopback-only
// (admin mux); not gated by a client cert. Refuses all calls after the first.
func (b *Broker) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	if b.bootstrapped {
		b.mu.Unlock()
		b.audit("bootstrap", b.manager, "deny", "already bootstrapped")
		http.Error(w, "already bootstrapped", http.StatusForbidden)
		return
	}
	b.bootstrapped = true
	b.mu.Unlock()

	ticket, err := b.tickets.Issue(b.manager, b.ticketTTL)
	if err != nil {
		b.audit("bootstrap", b.manager, "error", err.Error())
		http.Error(w, "mint failed", http.StatusInternalServerError)
		return
	}
	b.audit("bootstrap", b.manager, "allow", "")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"ticket": ticket})
}
