package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mcpCall records a single MCPAdd invocation.
type mcpCall struct {
	name string
	argv []string
}

func TestBootEnrolsAndConfigures(t *testing.T) {
	env := testBroker(t)
	ticket := provisionAs(t, env.Broker, env.Server, env.CA, "worker")
	dir := t.TempDir()
	bsPath := filepath.Join(dir, "bootstrap.json")
	bs, _ := json.Marshal(Bootstrap{Ticket: ticket, BrokerCA: string(env.CA.CertPEM()), BrokerURL: env.Server.URL, AgentCN: "worker"})
	_ = os.WriteFile(bsPath, bs, 0o600)

	var envOverlay map[string]string
	var calls []mcpCall
	idDir := filepath.Join(dir, "id")
	err := Boot(context.Background(), BootConfig{
		BootstrapPath: bsPath, IDDir: idDir, BrokerTools: []string{"db"}, Now: time.Now(),
		MCPAdd: func(name string, argv ...string) error {
			calls = append(calls, mcpCall{name: name, argv: argv})
			return nil
		},
		WriteEnvOverlay: func(m map[string]string) error { envOverlay = m; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadIdentity(idDir); !ok {
		t.Fatal("boot must write the enrolled identity")
	}
	if envOverlay["CLAUDE_CODE_CLIENT_CERT"] == "" || envOverlay["CLAUDE_CODE_CLIENT_KEY"] == "" || envOverlay["NODE_EXTRA_CA_CERTS"] == "" {
		t.Fatalf("env overlay missing identity vars: %v", envOverlay)
	}

	// capability server + one per broker tool ("db").
	if len(calls) < 2 {
		t.Fatalf("expected mcp add for capability server + db, got %v", calls)
	}

	// Assert capability server is registered in stdio form (no --transport flag).
	capCall := calls[0]
	if capCall.name != "lever-capability" {
		t.Errorf("first MCPAdd must register lever-capability, got %q", capCall.name)
	}
	if len(capCall.argv) < 2 || capCall.argv[0] != "lever-agent" || capCall.argv[1] != "serve-capability" {
		t.Errorf("lever-capability must use stdio argv [lever-agent serve-capability], got %v", capCall.argv)
	}
	for _, a := range capCall.argv {
		if a == "--transport" || a == "http" {
			t.Errorf("lever-capability (stdio) must not use --transport http, got argv %v", capCall.argv)
		}
	}

	// Assert broker tool "db" is registered with --transport http + full broker URL.
	var dbCall *mcpCall
	for i := range calls {
		if calls[i].name == "db" {
			dbCall = &calls[i]
			break
		}
	}
	if dbCall == nil {
		t.Fatal("expected MCPAdd call for broker tool 'db'")
	}
	assertBrokerToolArgv(t, "db", dbCall.argv, env.Server.URL)
}

func TestBootIsIdempotent(t *testing.T) {
	env := testBroker(t)
	ticket := provisionAs(t, env.Broker, env.Server, env.CA, "worker")
	dir := t.TempDir()
	bsPath := filepath.Join(dir, "bootstrap.json")
	bs, _ := json.Marshal(Bootstrap{Ticket: ticket, BrokerCA: string(env.CA.CertPEM()), BrokerURL: env.Server.URL, AgentCN: "worker"})
	_ = os.WriteFile(bsPath, bs, 0o600)
	idDir := filepath.Join(dir, "id")

	// First boot: enrols and configures.
	var firstCalls []mcpCall
	cfg := BootConfig{
		BootstrapPath: bsPath, IDDir: idDir, Now: time.Now(),
		BrokerTools: []string{"db"},
		MCPAdd: func(name string, argv ...string) error {
			firstCalls = append(firstCalls, mcpCall{name: name, argv: argv})
			return nil
		},
		WriteEnvOverlay: func(map[string]string) error { return nil },
	}
	if err := Boot(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	id1, _ := LoadIdentity(idDir)

	// Second boot: the ticket is now burned; if boot tried to re-enrol it would fail.
	// Idempotency means it sees the valid cert and skips enrol.
	var secondCalls []mcpCall
	cfg2 := cfg
	cfg2.MCPAdd = func(name string, argv ...string) error {
		secondCalls = append(secondCalls, mcpCall{name: name, argv: argv})
		return nil
	}
	if err := Boot(context.Background(), cfg2); err != nil {
		t.Fatalf("second boot must skip enrol (idempotent), got: %v", err)
	}
	id2, _ := LoadIdentity(idDir)
	if string(id1.CertPEM) != string(id2.CertPEM) {
		t.Fatal("idempotent boot must not re-enrol / change the cert")
	}

	// Second boot must still register tools with the full broker URL (bootstrap
	// re-read from file on the skip-enrol path).
	var dbCall2 *mcpCall
	for i := range secondCalls {
		if secondCalls[i].name == "db" {
			dbCall2 = &secondCalls[i]
			break
		}
	}
	if dbCall2 == nil {
		t.Fatal("second (idempotent) boot must still MCPAdd broker tool 'db'")
	}
	assertBrokerToolArgv(t, "db", dbCall2.argv, env.Server.URL)
}

// contains reports whether s appears in ss.
func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func TestBootAutoDiscoversTools(t *testing.T) {
	env := testBroker(t)
	ticket := provisionAs(t, env.Broker, env.Server, env.CA, "worker")
	dir := t.TempDir()
	bsPath := filepath.Join(dir, "bootstrap.json")
	bs, _ := json.Marshal(Bootstrap{Ticket: ticket, BrokerCA: string(env.CA.CertPEM()), BrokerURL: env.Server.URL, AgentCN: "worker"})
	_ = os.WriteFile(bsPath, bs, 0o600)

	var mcpAdds []string
	idDir := filepath.Join(dir, "id")
	cfg := BootConfig{
		BootstrapPath:   bsPath,
		IDDir:           idDir,
		Now:             time.Now(),
		MCPAdd:          func(name string, argv ...string) error { mcpAdds = append(mcpAdds, name); return nil },
		WriteEnvOverlay: func(map[string]string) error { return nil },
		ListTools: func(_ context.Context, _ string, _ *http.Client) ([]string, error) {
			return []string{"db"}, nil
		},
	}
	if err := Boot(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if !contains(mcpAdds, "lever-capability") || !contains(mcpAdds, "db") {
		t.Fatalf("expected lever-capability + db registered, got %v", mcpAdds)
	}
}

// assertBrokerToolArgv checks that argv for a broker tool registration contains
// --transport http and the full broker URL path for the tool.
func assertBrokerToolArgv(t *testing.T, tool string, argv []string, brokerURL string) {
	t.Helper()
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--transport") {
		t.Errorf("broker tool %q MCPAdd argv must contain --transport, got %v", tool, argv)
	}
	if !strings.Contains(joined, "http") {
		t.Errorf("broker tool %q MCPAdd argv must contain http transport, got %v", tool, argv)
	}
	wantSuffix := brokerURL + "/mcp/" + tool + "/"
	if !strings.Contains(joined, wantSuffix) {
		t.Errorf("broker tool %q MCPAdd argv must contain %q, got %v", tool, wantSuffix, argv)
	}
	// Verify the exact flag sequence --transport http appears.
	found := false
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--transport" && argv[i+1] == "http" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("broker tool %q MCPAdd argv must have --transport http sequence, got %v", tool, argv)
	}
}
