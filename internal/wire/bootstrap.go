// Package wire holds the on-the-wire/on-disk envelope types shared across the
// host, the broker and the in-container agent — the ONE place their JSON
// contract is declared, so a tag can never drift between a producer and a
// consumer that live in different packages. It is a leaf: it imports nothing
// internal, so any package (including internal/agent, whose tests import
// internal/broker) can depend on it without an import cycle.
package wire

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Bootstrap is the enrolment envelope the host/manager (for the manager) or the
// broker (for a worker, and on auto-re-enrol) deposits as bootstrap.json for an
// agent's lever-agent boot to consume. Previously this 4-field struct was
// declared three times (agent, apply, broker) with hand-maintained,
// "must-stay-identical" json tags; this is now its single declaration.
type Bootstrap struct {
	Ticket    string `json:"ticket"`
	BrokerCA  string `json:"broker_ca"`
	BrokerURL string `json:"broker_url"`
	AgentCN   string `json:"agent_cn"`
}

// Stage writes b as <dir>/bootstrap.json — the deposit LoadBootstrap reads by
// convention — creating dir 0700 and the file 0600. The explicit Chmod after
// WriteFile is load-bearing on a re-stage: os.WriteFile applies its perm only
// when it CREATES the file, so overwriting an existing bootstrap.json would
// otherwise keep whatever mode it already had. This is the single staging path
// for the envelope, shared by the host (manager enrolment) and the broker
// (worker dispatch + auto-re-enrol, which re-stages a fresh ticket over the
// existing file on every resume).
func Stage(dir string, b Bootstrap) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("wire: stage bootstrap: mkdir: %w", err)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("wire: stage bootstrap: marshal: %w", err)
	}
	p := filepath.Join(dir, "bootstrap.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		return fmt.Errorf("wire: stage bootstrap: write: %w", err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		return fmt.Errorf("wire: stage bootstrap: chmod: %w", err)
	}
	return nil
}
