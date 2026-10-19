package broker

import (
	"net/http"

	"github.com/stevegeek/lever/internal/wire"
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

	// Sole: a heal earlier in this broker's life may have staged a manager
	// ticket that a discarded container copied before it went; the ticket
	// staged now must be the only one that still enrols as the manager.
	ticket, err := b.tickets.IssueSole(b.manager, b.ticketTTL)
	if err != nil {
		b.audit("bootstrap", b.manager, "error", err.Error())
		http.Error(w, "mint failed", http.StatusInternalServerError)
		return
	}
	b.audit("bootstrap", b.manager, "allow", "")
	writeJSON(w, wire.BootstrapResponse{Ticket: ticket})
}
