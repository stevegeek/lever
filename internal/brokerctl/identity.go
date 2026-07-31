package brokerctl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/stevegeek/lever/internal/config"
)

// ConfigHash digests the broker-relevant configuration: the broker block
// (tools, ports, knobs) and the workers list (worker specs feed the broker's
// dispatch table). apply's broker-reuse shortcut compares a running broker's
// reported hash against this to decide whether the broker must be restarted
// on re-apply (#19) — so the hash must cover exactly the config a running
// broker bakes in at start, and nothing else (a manager-image change must
// NOT bounce the broker).
func ConfigHash(app *config.App) string {
	j, err := json.Marshal(struct {
		Broker  config.Broker
		Workers []config.Worker
	}{app.Broker, app.Workers})
	if err != nil {
		// Marshal of plain config structs cannot fail in practice; an empty
		// hash makes the comparison a guaranteed mismatch (restart), which
		// fails toward the safe side.
		return ""
	}
	sum := sha256.Sum256(j)
	return hex.EncodeToString(sum[:])
}
