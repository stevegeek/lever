package broker

import (
	"encoding/json"
	"net/http"
)

// handleBumpEpoch raises the epoch floor (revoke-all). Admin/loopback only.
func (b *Broker) handleBumpEpoch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	b.BumpEpoch()
	b.audit("bump-epoch", "", "allow", "")
	w.WriteHeader(http.StatusOK)
}

// handleRevoke revokes one agent. Admin/loopback only. Body: {"agent":"<cn>"}.
func (b *Broker) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Agent string `json:"agent"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Agent == "" {
		b.audit("revoke", "", "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	b.Revoke(req.Agent)
	b.audit("revoke", req.Agent, "allow", "")
	w.WriteHeader(http.StatusOK)
}
