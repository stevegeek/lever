package broker

import (
	"net/http"

	"github.com/stevegeek/lever/internal/wire"
)

// handleBumpEpoch raises the epoch floor (revoke-all). Admin/loopback only.
func (b *Broker) handleBumpEpoch(w http.ResponseWriter, r *http.Request) {
	b.BumpEpoch()
	b.audit("bump-epoch", "", "allow", "")
	w.WriteHeader(http.StatusOK)
}

// handleRevoke revokes one agent. Admin/loopback only. Body: {"agent":"<cn>"}.
func (b *Broker) handleRevoke(w http.ResponseWriter, r *http.Request) {
	var req wire.RevokeRequest
	if err := decodeBody(w, r, smallBodyLimit, &req); err != nil || req.Agent == "" {
		b.audit("revoke", "", "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	b.Revoke(req.Agent)
	b.audit("revoke", req.Agent, "allow", "")
	w.WriteHeader(http.StatusOK)
}
