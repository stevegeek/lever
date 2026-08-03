package apply

import (
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/agent"
)

// TestBootstrapMaterialRoundTripsToAgent pins the manager-enrolment wire
// contract across the apply→agent package boundary: what apply stages must be
// what lever-agent boot loads. The two ends declare the envelope independently
// (apply.BootstrapMaterial vs agent.Bootstrap) with hand-maintained,
// supposedly byte-identical json tags. Nothing else in the suite exercises
// both ends together — apply/run_test.go only stages `{}` and checks the latch,
// and no test decodes a manager bootstrap.json against agent's reader — so a
// tag typo on either side passes CI today and breaks live manager enrolment.
// This test is that missing guard.
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

	// LoadBootstrap reads the concrete file StageBootstrapMaterial writes by
	// convention: treeDir/.lever/bootstrap.json.
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
