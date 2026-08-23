package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWriteSidecarSpecsAPIKey(t *testing.T) {
	home := t.TempDir()
	bsDir := filepath.Join(home, "ws", ".lever")
	if err := os.MkdirAll(bsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(bsDir, "bootstrap.json")
	const brokerURL = "https://host.orb.internal:8443"
	if err := os.WriteFile(bootstrap, []byte(`{"broker_url":"`+brokerURL+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	idDir := filepath.Join(home, ".lever-id")
	settings := filepath.Join(home, ".claude", "settings.json")

	if err := WriteSidecarSpecs(SidecarConfig{
		HomeDir: home, IDDir: idDir, BootstrapPath: bootstrap, SettingsPath: settings, LLMAuth: LLMAuthAPIKey,
	}); err != nil {
		t.Fatalf("WriteSidecarSpecs: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(home, ".scion", "scion-services.yaml"))
	if err != nil {
		t.Fatalf("read services file: %v", err)
	}
	var specs []sidecarSpec
	if err := yaml.Unmarshal(b, &specs); err != nil {
		t.Fatalf("parse services yaml: %v", err)
	}
	// Two sidecars: lever-gateway (the loopback mTLS proxy) then lever-renew.
	if len(specs) != 2 {
		t.Fatalf("want 2 services, got %d: %s", len(specs), b)
	}

	// Gateway MUST be emitted first (up before renew) and carry baked absolute
	// flags — sidecars get no CWD, so it must never fall back to a bootstrap path.
	gw := specs[0]
	if gw.Name != "lever-gateway" {
		t.Errorf("specs[0].name = %q, want lever-gateway (emitted first)", gw.Name)
	}
	if gw.Restart != "on-failure" {
		t.Errorf("gateway restart = %q, want on-failure", gw.Restart)
	}
	gwCmd := strings.Join(gw.Command, " ")
	for _, want := range []string{
		"lever-agent gateway",
		"--id-dir " + idDir,
		"--broker-url " + brokerURL, // baked; no sidecar bootstrap file-read
		"--listen 127.0.0.1:8462",
	} {
		if !strings.Contains(gwCmd, want) {
			t.Errorf("gateway command %q missing %q", gwCmd, want)
		}
	}

	s := specs[1]
	if s.Name != "lever-renew" {
		t.Errorf("specs[1].name = %q, want lever-renew", s.Name)
	}
	if s.Restart != "on-failure" {
		t.Errorf("restart = %q, want on-failure", s.Restart)
	}
	cmd := strings.Join(s.Command, " ")
	for _, want := range []string{
		"lever-agent renew --loop",
		"--id-dir " + idDir,
		"--broker-url " + brokerURL, // resolved at boot; no sidecar file-read
		"--llm-auth api-key",
		"--settings " + settings,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command %q missing %q", cmd, want)
		}
	}
}

// TestWriteSidecarSpecsNoBootstrapIsNoop: a non-brokered agent (no bootstrap
// file) gets no sidecar — there is nothing to renew against.
func TestWriteSidecarSpecsNoBootstrapIsNoop(t *testing.T) {
	home := t.TempDir()
	missing := filepath.Join(home, "ws", ".lever", "bootstrap.json")
	if err := WriteSidecarSpecs(SidecarConfig{
		HomeDir: home, IDDir: filepath.Join(home, ".lever-id"), BootstrapPath: missing, LLMAuth: LLMAuthSubscription,
	}); err != nil {
		t.Fatalf("WriteSidecarSpecs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".scion", "scion-services.yaml")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("services file should not exist for a non-brokered agent; stat err = %v", err)
	}
}

// TestWriteSidecarSpecsEmptyBrokerURLIsNoop: a bootstrap that exists but carries
// no broker URL (brokerless) is a distinct path from a missing bootstrap — it too
// gets no sidecar, since there is nothing to renew against.
func TestWriteSidecarSpecsEmptyBrokerURLIsNoop(t *testing.T) {
	home := t.TempDir()
	bsDir := filepath.Join(home, "ws", ".lever")
	if err := os.MkdirAll(bsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(bsDir, "bootstrap.json")
	if err := os.WriteFile(bootstrap, []byte(`{"ticket":"tk","broker_url":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteSidecarSpecs(SidecarConfig{
		HomeDir: home, IDDir: filepath.Join(home, ".lever-id"), BootstrapPath: bootstrap, LLMAuth: LLMAuthAPIKey,
	}); err != nil {
		t.Fatalf("WriteSidecarSpecs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".scion", "scion-services.yaml")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("services file should not exist for a brokerless bootstrap; stat err = %v", err)
	}
}
