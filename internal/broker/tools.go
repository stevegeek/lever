package broker

import (
	"net/http"

	"github.com/stevegeek/lever/internal/wire"
)

// handleTools returns the broker's registered tool names to an authenticated
// agent (mTLS). It is the FULL catalog, not policy-filtered: an agent may call a
// tool with a delegated token even without a direct grant, so filtering by
// the MayObtainRule policy would wrongly hide such tools. The token + mTLS are
// the real gate.
func (b *Broker) handleTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// A revoked agent cannot enumerate the tool catalog (the last read-only
	// path — completes "revocation denies every acting/observing path").
	if _, ok := b.requireLiveAgent(w, r, "tools", ""); !ok {
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
	writeJSON(w, wire.ToolsResponse{Tools: out})
}
