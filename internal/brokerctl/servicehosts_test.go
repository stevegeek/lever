package brokerctl

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/servicehost"
	"github.com/stevegeek/lever/internal/state"
)

func serviceHostApp(tree string) *config.App {
	return &config.App{Tree: tree, Broker: config.Broker{
		ServiceHosts: []config.ServiceHost{{Name: "laptop", Address: "laptop:7443",
			Fingerprint: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}},
		Tools: []config.Tool{{Name: "mail", Host: "laptop", External: true}, {Name: "db", Command: []string{"x"}, Backend: "127.0.0.1:3201"}},
	}}
}

func TestServiceHostRoutes(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "workspace")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	st := state.ForConfig(root)
	app := serviceHostApp(tree)

	if _, err := ServiceHostRoutes(app, st); err == nil || !strings.Contains(err.Error(), "lever service-host pair laptop") {
		t.Fatalf("unpaired: %v, want the pair hint", err)
	}
	if _, _, err := servicehost.EnsureKeyPair(st.ServiceHostClientCert("laptop"), st.ServiceHostClientKey("laptop"), "b", x509.ExtKeyUsageClientAuth, false); err != nil {
		t.Fatal(err)
	}
	routes, err := ServiceHostRoutes(app, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes["mail"].Host != "laptop" || routes["mail"].Transport == nil {
		t.Fatalf("routes = %+v", routes)
	}
	if err := os.Chmod(st.ServiceHostClientKey("laptop"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ServiceHostRoutes(app, st); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a readable client key: %v", err)
	}
	if _, err := ServiceHostRoutes(serviceHostApp(root), st); err == nil || !strings.Contains(err.Error(), "inside the tree") {
		t.Fatalf("state inside the tree: %v", err)
	}
	if routes, err := ServiceHostRoutes(&config.App{Tree: root}, st); err != nil || routes != nil {
		t.Fatalf("no service hosts: %v %v", routes, err)
	}
}

func TestBuildBrokerRoutesHostToolsToTheServiceHost(t *testing.T) {
	app := serviceHostApp(t.TempDir())
	app.Broker.Tools[0].Operations = []config.Op{{Name: "search"}}
	app.Broker.Tools[1].Operations = []config.Op{{Name: "read"}}
	app.Broker.LLMAuth = config.LLMAuthSubscription
	kp, _ := token.Generate()
	caInst, _ := ca.Generate()
	cfg, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		t.Fatal(err)
	}
	tl, ok := cfg.Identity.Registry.Lookup("mail")
	if !ok || tl.Backend != "https://laptop:7443/mcp/mail" || tl.FirstParty || !tl.External {
		t.Fatalf("mail = %+v", tl)
	}
}
