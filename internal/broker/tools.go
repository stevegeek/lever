package broker

import (
	"net/http"

	"github.com/stevegeek/lever/internal/wire"
)

// handleTools returns the FULL catalog, not policy-filtered: an agent may
// call a tool with a delegated token and no direct grant. The token and mTLS
// are the real gate.
func (b *Broker) handleTools(w http.ResponseWriter, r *http.Request) {
	// Revocation denies every acting and observing path, this one included.
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
