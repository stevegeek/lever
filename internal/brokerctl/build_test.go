package brokerctl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
	"github.com/lever-to/lever/internal/config"
)

func sampleApp() *config.App {
	return &config.App{
		Name:    "demo",
		Backend: "orbstack",
		Manager: config.Manager{
			Image:    "scionlocal/mgr",
			Delegate: []config.DelegateGrant{{Tool: "db", Op: "read", To: []string{"worker"}}},
		},
		Groves: []config.Grove{{Name: "worker", Dir: "work"}},
		Broker: config.Broker{
			JailPort:  8443,
			AdminPort: 8444,
			Tools: []config.Tool{{
				Name: "db", Command: []string{"lever-tool-db"}, Backend: "127.0.0.1:3201",
				Operations:    []config.Op{{Name: "read", CaveatParam: map[string]string{"table": "table"}}},
				AllowedValues: map[string][]string{"table": {"A", "B"}},
			}},
		},
	}
}

func TestBuildBrokerAssemblesRulesAndRegistry(t *testing.T) {
	kp, _ := token.Generate()
	caInst, _ := ca.Generate()
	cfg, err := BuildBroker(sampleApp(), kp, caInst, ca.NewTicketStore())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Rules.MayObtain("manager", "worker", "db", "read") {
		t.Fatal("manager must be allowed to delegate db.read to worker")
	}
	if cfg.Rules.MayObtain("worker", "worker", "db", "read") {
		t.Fatal("worker has no obtain grant — must be denied a self-path")
	}
	tool, ok := cfg.Registry.Lookup("db")
	if !ok || tool.Backend != "127.0.0.1:3201" || !tool.FirstParty {
		t.Fatalf("registry envelope wrong: %+v ok=%v", tool, ok)
	}
	if cfg.ManagerIdentity != "manager" || len(cfg.Agents) != 1 || cfg.Agents[0] != "worker" {
		t.Fatalf("identity/agents wrong: %q %v", cfg.ManagerIdentity, cfg.Agents)
	}
}

func TestBuildBrokerRegistersLLMPseudoToolForAPIKey(t *testing.T) {
	kp, _ := token.Generate()
	caInst, _ := ca.Generate()
	keyPath := writeKeyFile(t, "sk-ant-test", 0o600)
	app := &config.App{
		Broker:  config.Broker{LLMAuth: config.LLMAuthAPIKey, APIKeyFile: keyPath},
		Manager: config.Manager{Obtain: []config.Grant{{Tool: "llm", Op: "generate"}}},
	}
	bc, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !bc.Registry.HasOperation("llm", "generate") {
		t.Fatal("api-key build: registry missing llm/generate")
	}
}

func TestBuildBrokerNoLLMToolForSubscription(t *testing.T) {
	kp, _ := token.Generate()
	caInst, _ := ca.Generate()
	app := &config.App{Broker: config.Broker{LLMAuth: config.LLMAuthSubscription}}
	bc, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if bc.Registry.HasOperation("llm", "generate") {
		t.Fatal("subscription build: registry must not register llm")
	}
}

// writeKeyFile writes content to a temp file with the given perm and returns the path.
func writeKeyFile(t *testing.T, content string, perm os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "api_key")
	if err := os.WriteFile(p, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	// Explicitly set the mode (WriteFile may not honour it reliably with umask).
	if err := os.Chmod(p, perm); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildBrokerLoadsAPIKey(t *testing.T) {
	kp, _ := token.Generate()
	caInst, _ := ca.Generate()
	keyPath := writeKeyFile(t, "sk-test\n", 0o600)
	app := &config.App{
		Broker: config.Broker{
			LLMAuth:    config.LLMAuthAPIKey,
			APIKeyFile: keyPath,
		},
		Manager: config.Manager{Obtain: []config.Grant{{Tool: "llm", Op: "generate"}}},
	}
	bc, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		t.Fatalf("BuildBroker: %v", err)
	}
	if string(bc.APIKey) != "sk-test" {
		t.Fatalf("expected APIKey %q, got %q", "sk-test", string(bc.APIKey))
	}
}

func TestBuildBrokerRejectsNonSecretKey(t *testing.T) {
	kp, _ := token.Generate()
	caInst, _ := ca.Generate()
	keyPath := writeKeyFile(t, "sk-test", 0o644)
	app := &config.App{
		Broker: config.Broker{
			LLMAuth:    config.LLMAuthAPIKey,
			APIKeyFile: keyPath,
		},
	}
	_, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err == nil {
		t.Fatal("expected error for 0644 api_key_file, got nil")
	}
}

func TestBuildBrokerNoAPIKeyForSubscription(t *testing.T) {
	kp, _ := token.Generate()
	caInst, _ := ca.Generate()
	app := &config.App{Broker: config.Broker{LLMAuth: config.LLMAuthSubscription}}
	bc, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		t.Fatalf("BuildBroker: %v", err)
	}
	if len(bc.APIKey) != 0 {
		t.Fatal("subscription build must not populate APIKey")
	}
}

func TestBuildBrokerDeepCopiesMaps(t *testing.T) {
	app := sampleApp()
	kp, _ := token.Generate()
	caInst, _ := ca.Generate()
	cfg, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the source config must not affect the registered envelope.
	app.Broker.Tools[0].AllowedValues["table"][0] = "MUTATED"
	tool, _ := cfg.Registry.Lookup("db")
	if tool.AllowedValues["table"][0] == "MUTATED" {
		t.Fatal("registry aliased the config slice — must deep-copy (registry takes ownership)")
	}
}
