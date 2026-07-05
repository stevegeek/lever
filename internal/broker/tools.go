package broker

import (
	"encoding/json"
	"net/http"

	"github.com/lever-to/lever/internal/cap/ca"
)

// handleTools returns the broker's registered tool names to an authenticated
// agent (mTLS). It is the FULL catalog, not policy-filtered: an agent may call a
// tool with a delegated token even without a direct grant, so filtering by
// MayObtain would wrongly hide such tools. The token + mTLS are the real gate.
func (b *Broker) handleTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	caller, err := ca.RequireAgent(r)
	if err != nil {
		b.audit("tools", "", "deny", err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// A revoked agent cannot enumerate the tool catalog (the last read-only
	// path — completes "revocation denies every acting/observing path").
	if b.isRevoked(caller) {
		b.audit("tools", caller, "deny", "revoked")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	names := b.reg.Names()
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n == ReservedLLMTool {
			continue
		}
		out = append(out, n)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string][]string{"tools": out})
}
