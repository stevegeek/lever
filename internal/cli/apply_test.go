package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/config"
)

// writeTmpConfig writes a minimal app.yaml with a real tree directory structure
// and returns the config file path. Mirrors config_test.go's writeTmp.
func writeTmpConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "groves", "worker"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `name: demo
backend: orbstack
tree: ./tree
broker:
  llm_auth: subscription
manager:
  image: scionlocal/lever-claude:latest
  allow_ports: [3305]
groves:
  - name: worker
    dir: groves/worker
`
	p := filepath.Join(dir, "app.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Egress is an explicit posture, decoupled from llm_auth: closed only when
// `egress: closed` is set; api-key alone leaves egress open.
func TestApplyEgressPostureFromConfig(t *testing.T) {
	closedApp := &config.App{Egress: config.EgressClosed, Broker: config.Broker{LLMAuth: config.LLMAuthAPIKey, JailPort: 8443}}
	if closed, warn := closedApp.ClosedInternetEgress(); !closed || warn != "" {
		t.Fatalf("egress: closed must resolve closed egress: closed=%v warn=%q", closed, warn)
	}
	openApp := &config.App{Broker: config.Broker{LLMAuth: config.LLMAuthAPIKey, JailPort: 8443}}
	if closed, _ := openApp.ClosedInternetEgress(); closed {
		t.Fatal("api-key WITHOUT egress: closed must leave egress open (decoupled)")
	}
}

// TestApplyOpenEgressForSubscription verifies that a subscription instance does
// not set the closed posture (and emits no warning since it's a pure subscription).
func TestApplyOpenEgressForSubscription(t *testing.T) {
	app := &config.App{Broker: config.Broker{LLMAuth: config.LLMAuthSubscription, JailPort: 8443}}
	closed, warn := app.ClosedInternetEgress()
	if closed {
		t.Fatalf("subscription instance must not resolve closed egress")
	}
	if warn != "" {
		t.Fatalf("pure subscription must not produce warning; got %q", warn)
	}
}

func TestApplyDryRun(t *testing.T) {
	p := writeTmpConfig(t)

	// newApplyCmd with nil BackendFactory is safe for --dry-run: the backend
	// is never touched in dry-run mode (plan is printed and the func returns).
	cmd := newApplyCmd(nil)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{p, "--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "jail-up") {
		t.Errorf("dry-run output should contain 'jail-up'; got:\n%s", got)
	}
	if !strings.Contains(got, "start-manager") {
		t.Errorf("dry-run output should contain 'start-manager'; got:\n%s", got)
	}
}

func TestApplyDryRunDiscoversConfig(t *testing.T) {
	dir := instanceDir(t, "demo")
	t.Chdir(dir)

	cmd := newApplyCmd(nil) // nil backend safe for --dry-run
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--dry-run"}) // NO config arg — discovered from cwd

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "jail-up") || !strings.Contains(got, "start-manager") {
		t.Errorf("dry-run via discovery produced:\n%s", got)
	}
}
