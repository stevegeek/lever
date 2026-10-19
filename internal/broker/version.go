package broker

import (
	"net/http"

	"github.com/stevegeek/lever/internal/jailabi"
	"github.com/stevegeek/lever/internal/wire"
)

// handleVersion answers the host lever's build and Jail ABI, so an agent can
// tell whether its image matches the host. It needs a live agent identity and
// no capability. It answers those two values only: no instance id, config
// hash, path or caller detail.
func (b *Broker) handleVersion(w http.ResponseWriter, r *http.Request) {
	if _, ok := b.requireLiveAgent(w, r, "version", ""); !ok {
		return
	}
	writeJSON(w, wire.VersionResponse{Version: b.version, JailABI: jailabi.JailABI})
}
