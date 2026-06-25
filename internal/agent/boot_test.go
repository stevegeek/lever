package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBootEnrolsAndConfigures(t *testing.T) {
	env := testBroker(t)
	ticket := provisionAs(t, env.Broker, env.Server, env.CA, "worker")
	dir := t.TempDir()
	bsPath := filepath.Join(dir, "bootstrap.json")
	bs, _ := json.Marshal(Bootstrap{Ticket: ticket, BrokerCA: string(env.CA.CertPEM()), BrokerURL: env.Server.URL, AgentCN: "worker"})
	_ = os.WriteFile(bsPath, bs, 0o600)

	var envOverlay map[string]string
	var mcpAdds []string
	idDir := filepath.Join(dir, "id")
	err := Boot(context.Background(), BootConfig{
		BootstrapPath: bsPath, IDDir: idDir, BrokerTools: []string{"db"}, Now: time.Now(),
		MCPAdd:          func(name string, argv ...string) error { mcpAdds = append(mcpAdds, name); return nil },
		WriteEnvOverlay: func(m map[string]string) error { envOverlay = m; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadIdentity(idDir); !ok {
		t.Fatal("boot must write the enrolled identity")
	}
	if envOverlay["CLAUDE_CODE_CLIENT_CERT"] == "" || envOverlay["NODE_EXTRA_CA_CERTS"] == "" {
		t.Fatalf("env overlay missing identity vars: %v", envOverlay)
	}
	// capability server + one per broker tool ("db").
	if len(mcpAdds) < 2 {
		t.Fatalf("expected mcp add for capability server + db, got %v", mcpAdds)
	}
}

func TestBootIsIdempotent(t *testing.T) {
	env := testBroker(t)
	ticket := provisionAs(t, env.Broker, env.Server, env.CA, "worker")
	dir := t.TempDir()
	bsPath := filepath.Join(dir, "bootstrap.json")
	bs, _ := json.Marshal(Bootstrap{Ticket: ticket, BrokerCA: string(env.CA.CertPEM()), BrokerURL: env.Server.URL, AgentCN: "worker"})
	_ = os.WriteFile(bsPath, bs, 0o600)
	idDir := filepath.Join(dir, "id")
	cfg := BootConfig{BootstrapPath: bsPath, IDDir: idDir, Now: time.Now(),
		MCPAdd: func(string, ...string) error { return nil }, WriteEnvOverlay: func(map[string]string) error { return nil }}
	if err := Boot(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	id1, _ := LoadIdentity(idDir)
	// Second boot: the ticket is now burned; if boot tried to re-enrol it would fail.
	// Idempotency means it sees the valid cert and skips enrol.
	if err := Boot(context.Background(), cfg); err != nil {
		t.Fatalf("second boot must skip enrol (idempotent), got: %v", err)
	}
	id2, _ := LoadIdentity(idDir)
	if string(id1.CertPEM) != string(id2.CertPEM) {
		t.Fatal("idempotent boot must not re-enrol / change the cert")
	}
}
