package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/broker/brokertest"
)

func writeBootstrap(t *testing.T, env *brokertest.Env, dir string) string {
	t.Helper()
	ticket := env.WorkerTicket(t, "worker")
	bsPath := filepath.Join(dir, "bootstrap.json")
	bs, _ := json.Marshal(Bootstrap{
		Ticket:    ticket,
		BrokerCA:  string(env.CA.CertPEM()),
		BrokerURL: env.Server.URL,
		AgentCN:   "worker",
	})
	if err := os.WriteFile(bsPath, bs, 0o600); err != nil {
		t.Fatal(err)
	}
	return bsPath
}

func baseBootConfig(t *testing.T, env *brokertest.Env) BootConfig {
	t.Helper()
	dir := t.TempDir()
	return BootConfig{
		BootstrapPath: writeBootstrap(t, env, dir),
		IDDir:         filepath.Join(dir, "id"),
		BrokerTools:   []string{"db"},
		SettingsPath:  filepath.Join(dir, "settings.json"),
		MCPAdd:        func(string, ...string) error { return nil },
	}
}

func TestBootAPIKeyWritesAnthropicEnv(t *testing.T) {
	env := testBroker(t)
	allowLLM(t, env, "worker")
	c := baseBootConfig(t, env)
	c.LLMAuth = LLMAuthAPIKey
	if err := Boot(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	overlay := readSettingsEnv(t, c.SettingsPath)
	if overlay["ANTHROPIC_AUTH_TOKEN"] == "" {
		t.Error("api-key boot must write ANTHROPIC_AUTH_TOKEN")
	}
	if want := LocalGatewayURL + "/llm"; overlay["ANTHROPIC_BASE_URL"] != want {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want %q (must point at the loopback gateway, not the broker)", overlay["ANTHROPIC_BASE_URL"], want)
	}
	if overlay["CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN"] != "1" {
		t.Errorf("harness overlay keys missing from settings env: %v", overlay)
	}
}

func TestBootAPIKeyFailsClosedOnTokenError(t *testing.T) {
	// Fail closed: with no llm obtain grant, Boot must abort before writing the
	// settings env rather than leave a blank ANTHROPIC_AUTH_TOKEN.
	env := testBroker(t)
	c := baseBootConfig(t, env)
	c.LLMAuth = LLMAuthAPIKey
	if err := Boot(context.Background(), c); err == nil {
		t.Fatal("Boot must fail closed when the llm token cannot be obtained")
	}
	if _, err := os.Stat(c.SettingsPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Boot must NOT write settings.json after a failed llm-token acquisition; stat err = %v", err)
	}
}

func TestBootSubscriptionOmitsAnthropicAuthToken(t *testing.T) {
	env := testBroker(t)
	c := baseBootConfig(t, env)
	c.LLMAuth = LLMAuthSubscription
	if err := Boot(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	overlay := readSettingsEnv(t, c.SettingsPath)
	if _, ok := overlay["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Error("subscription boot must NOT set ANTHROPIC_AUTH_TOKEN")
	}
	if overlay["CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN"] != "1" {
		t.Errorf("subscription boot must still write the harness overlay: %v", overlay)
	}
}

func TestBootEnrolOnlySkipsSettingsAndMCP(t *testing.T) {
	env := testBroker(t)
	dir := t.TempDir()
	c := BootConfig{BootstrapPath: writeBootstrap(t, env, dir), IDDir: filepath.Join(dir, "id")}
	if err := Boot(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadIdentity(c.IDDir); !ok {
		t.Fatal("enrol-only boot must still write the identity")
	}
}

type mcpCall struct {
	name string
	argv []string
}

func TestBootEnrolsAndConfigures(t *testing.T) {
	env := testBroker(t)
	var calls []mcpCall
	c := baseBootConfig(t, env)
	c.MCPAdd = func(name string, argv ...string) error {
		calls = append(calls, mcpCall{name: name, argv: argv})
		return nil
	}
	if err := Boot(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadIdentity(c.IDDir); !ok {
		t.Fatal("boot must write the enrolled identity")
	}
	if envOverlay := readSettingsEnv(t, c.SettingsPath); envOverlay["CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN"] != "1" {
		t.Fatalf("settings env missing the harness overlay: %v", envOverlay)
	}

	if len(calls) < 2 {
		t.Fatalf("expected mcp add for capability server + db, got %v", calls)
	}

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
	assertBrokerToolArgv(t, "db", dbCall.argv, LocalGatewayURL)
	// The MCP URL must be the gateway, never the broker directly.
	if strings.Contains(strings.Join(dbCall.argv, " "), env.Server.URL) {
		t.Errorf("broker tool 'db' MCPAdd must route via the gateway, not the broker URL %q: %v", env.Server.URL, dbCall.argv)
	}
}

func TestBootIsIdempotent(t *testing.T) {
	env := testBroker(t)

	cfg := baseBootConfig(t, env)
	idDir := cfg.IDDir
	if err := Boot(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	id1, _ := LoadIdentity(idDir)

	// The ticket is burned, so a second boot succeeds only by skipping enrol.
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

	// The skip-enrol path must still read the broker URL from the bootstrap.
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
	assertBrokerToolArgv(t, "db", dbCall2.argv, LocalGatewayURL)
}

// TestBootDiscoveryUsesBrokerNotGateway: the gateway sidecar is not up during
// pre-start, so discovery must go straight to the broker.
func TestBootDiscoveryUsesBrokerNotGateway(t *testing.T) {
	// No gateway is listening on LocalGatewayAddr during this test, so a
	// discovery that succeeds proves it went straight to the broker over mTLS.
	env := testBroker(t)
	regDB(t, env)
	var mcpAdds []string
	cfg := baseBootConfig(t, env)
	cfg.BrokerTools = nil
	cfg.DiscoverTools = true
	cfg.MCPAdd = func(name string, argv ...string) error { mcpAdds = append(mcpAdds, name); return nil }
	if err := Boot(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(mcpAdds, "lever-capability") || !slices.Contains(mcpAdds, "db") {
		t.Fatalf("expected lever-capability + discovered db registered, got %v", mcpAdds)
	}
}

func TestBootExplicitToolsSkipDiscovery(t *testing.T) {
	// -tools "" (explicit empty list) must not fall back to discovery: the
	// registry here has "db", but nothing may be registered.
	env := testBroker(t)
	regDB(t, env)
	var mcpAdds []string
	cfg := baseBootConfig(t, env)
	cfg.BrokerTools = nil
	cfg.DiscoverTools = false
	cfg.MCPAdd = func(name string, argv ...string) error { mcpAdds = append(mcpAdds, name); return nil }
	if err := Boot(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(mcpAdds, "db") {
		t.Fatalf("explicit empty tool list must skip discovery, got %v", mcpAdds)
	}
}

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

func TestLoadBootstrapNormalizesBrokerURL(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bootstrap.json")
	if err := os.WriteFile(p, []byte(`{"broker_url":"https://broker:8443//"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bs, err := LoadBootstrap(p)
	if err != nil {
		t.Fatal(err)
	}
	if bs.BrokerURL != "https://broker:8443" {
		t.Fatalf("BrokerURL = %q, want trailing slashes stripped", bs.BrokerURL)
	}
	if got := NormalizeBrokerURL("https://b/"); got != "https://b" {
		t.Fatalf("NormalizeBrokerURL = %q", got)
	}
}

// boot may read the bootstrap as root from the agent-writable tree: a symlink
// at .lever or at bootstrap.json is refused by name.
func TestLoadBootstrapRefusesSymlink(t *testing.T) {
	ws := t.TempDir()
	real := filepath.Join(ws, "real.json")
	if err := os.WriteFile(real, []byte(`{"broker_url":"https://broker.example"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(ws, ".lever")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "bootstrap.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBootstrap(filepath.Join(dir, "bootstrap.json")); !errors.Is(err, errRefusedPath) {
		t.Fatalf("LoadBootstrap through a symlink: err = %v, want errRefusedPath", err)
	}
	// WriteSidecarSpecs relies on fs.ErrNotExist for an absent bootstrap.
	if _, err := LoadBootstrap(filepath.Join(ws, "missing", "bootstrap.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent bootstrap: err = %v, want fs.ErrNotExist", err)
	}
}

// A worker's bootstrap is the read-only /run/lever/bootstrap.json mount; the
// same confinement still refuses a link at either component.
func TestLoadBootstrapReadsMountedWorkerTicket(t *testing.T) {
	root := t.TempDir() // stands in for "/"
	dir := filepath.Join(root, "run", "lever")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "bootstrap.json")
	if err := os.WriteFile(p, []byte(`{"ticket":"t","broker_url":"https://broker:8443/","agent_cn":"worker"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bs, err := LoadBootstrap(p)
	if err != nil {
		t.Fatalf("LoadBootstrap(mounted ticket): %v", err)
	}
	if bs.Ticket != "t" || bs.AgentCN != "worker" || bs.BrokerURL != "https://broker:8443" {
		t.Fatalf("bootstrap = %+v", bs)
	}
	// The mount directory itself replaced by a link is refused, as is the file.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "bootstrap.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBootstrap(p); !errors.Is(err, errRefusedPath) {
		t.Fatalf("LoadBootstrap through a linked mount dir: err = %v, want errRefusedPath", err)
	}
}
