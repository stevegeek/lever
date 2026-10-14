package apply

import (
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/agent"
)

// TestBootstrapMaterialRoundTripsToAgent pins the manager-enrolment wire
// contract: apply.BootstrapMaterial and agent.Bootstrap declare the envelope
// independently with hand-maintained json tags, and nothing else exercises
// both ends together, so a tag typo would otherwise break live enrolment.
func TestBootstrapMaterialRoundTripsToAgent(t *testing.T) {
	treeDir := t.TempDir()

	want := BootstrapMaterial{
		Ticket:    "tkt-round-trip",
		BrokerCA:  "-----BEGIN CERTIFICATE-----\nMIIBcap\n-----END CERTIFICATE-----",
		BrokerURL: "https://broker.lever.local:8461",
		AgentCN:   "manager.example",
	}

	if err := StageBootstrapMaterial(treeDir, want); err != nil {
		t.Fatalf("StageBootstrapMaterial: %v", err)
	}

	got, err := agent.LoadBootstrap(filepath.Join(treeDir, ".lever", "bootstrap.json"))
	if err != nil {
		t.Fatalf("agent.LoadBootstrap: %v", err)
	}

	if got.Ticket != want.Ticket {
		t.Errorf("Ticket: got %q, want %q", got.Ticket, want.Ticket)
	}
	if got.BrokerCA != want.BrokerCA {
		t.Errorf("BrokerCA: got %q, want %q", got.BrokerCA, want.BrokerCA)
	}
	if got.BrokerURL != want.BrokerURL {
		t.Errorf("BrokerURL: got %q, want %q", got.BrokerURL, want.BrokerURL)
	}
	if got.AgentCN != want.AgentCN {
		t.Errorf("AgentCN: got %q, want %q", got.AgentCN, want.AgentCN)
	}
}
