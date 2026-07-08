package brokerctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/config"
)

func sampleApp() *config.App {
	return &config.App{
		Name:    "demo",
		Backend: "orbstack",
		Manager: config.Manager{
			Image:    "scionlocal/mgr",
			Delegate: []config.DelegateGrant{{Tool: "db", Op: "read", To: []string{"worker"}}},
		},
		Workers: []config.Worker{{Name: "worker", Dir: "work"}},
		Broker: config.Broker{
			LLMAuth:   config.LLMAuthSubscription,
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

func TestBuildBrokerRejectsEmptyAPIKeyFile(t *testing.T) {
	kp, _ := token.Generate()
	caInst, _ := ca.Generate()
	for _, content := range []string{"", "   ", "\n", "\t\n"} {
		keyPath := writeKeyFile(t, content, 0o600)
		app := &config.App{
			Broker: config.Broker{
				LLMAuth:    config.LLMAuthAPIKey,
				APIKeyFile: keyPath,
			},
		}
		_, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
		if err == nil {
			t.Errorf("content=%q: expected error for empty api_key_file, got nil", content)
			continue
		}
		if !strings.Contains(err.Error(), "empty") {
			t.Errorf("content=%q: error must mention \"empty\", got: %v", content, err)
		}
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

func TestBuildBrokerRegistersExternalTools(t *testing.T) {
	kp, _ := token.Generate()
	caInst, _ := ca.Generate()
	app := sampleApp()
	app.Broker.Tools = append(app.Broker.Tools,
		config.Tool{
			Name: "devonthink", External: true, Backend: "127.0.0.1:3302",
			Operations:    []config.Op{{Name: "search"}},
			AllowedValues: map[string][]string{"database": {"work"}},
		},
		config.Tool{Name: "things3", External: true, Backend: "127.0.0.1:3300", Gate: config.GateCoarse},
	)
	cfg, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		t.Fatal(err)
	}
	dt, ok := cfg.Registry.Lookup("devonthink")
	if !ok || dt.FirstParty || !dt.External || dt.Coarse {
		t.Fatalf("devonthink envelope = %+v ok=%v; want external fine, NOT first-party", dt, ok)
	}
	if !cfg.Registry.HasOperation("devonthink", "search") || cfg.Registry.HasOperation("devonthink", registry.WildcardOp) {
		t.Fatal("fine external tool must expose exactly its declared ops — and never the wildcard")
	}
	th, ok := cfg.Registry.Lookup("things3")
	if !ok || th.FirstParty || !th.External || !th.Coarse {
		t.Fatalf("things3 envelope = %+v ok=%v; want external coarse, NOT first-party", th, ok)
	}
	if !cfg.Registry.HasOperation("things3", registry.WildcardOp) {
		t.Fatal("coarse tool must expose the wildcard op (the /request mint path gates on HasOperation)")
	}
	db, _ := cfg.Registry.Lookup("db")
	if !db.FirstParty || db.External {
		t.Fatalf("supervised tool envelope changed: %+v", db)
	}
}
