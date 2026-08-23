package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stevegeek/lever/internal/scion/layout"
	"gopkg.in/yaml.v3"
)

// sidecarSpec mirrors the subset of scion's api.ServiceSpec we emit (scion
// unmarshals scion-services.yaml into []api.ServiceSpec). Local copy to avoid a
// dependency on the vendored scion module; the yaml keys must match.
type sidecarSpec struct {
	Name    string   `yaml:"name"`
	Command []string `yaml:"command"`
	Restart string   `yaml:"restart"`
}

// SidecarConfig drives WriteSidecarSpecs. Every path is baked into the sidecar
// argv verbatim, so callers pass absolute paths (scion does not set the
// sidecar's working directory).
type SidecarConfig struct {
	HomeDir       string // $HOME of the agent user; the spec lands in HomeDir/.scion/
	IDDir         string // identity directory both sidecars read
	BootstrapPath string // bootstrap.json the broker URL is resolved from, once, here
	SettingsPath  string // claude settings.json the renew sidecar rewrites in api-key mode
	LLMAuth       string // LLM-auth mode handed to the renew sidecar
}

// WriteSidecarSpecs emits $HOME/.scion/scion-services.yaml describing the two
// long-lived agent sidecars — lever-gateway (the loopback mTLS proxy Claude talks
// to) and lever-renew (auto-refresh of the agent cert and, in api-key mode, the
// LLM capability token) — so scion launches them right after the pre-start hooks.
// scion reads this file as []api.ServiceSpec and launches each entry as the agent
// user with the container env inherited (so the projected LEVER_LLM_AUTH still
// flows), but it does NOT set the sidecar's working directory — so every path here
// is made absolute and the broker URL is resolved from an explicit --bootstrap
// rather than a CWD-relative default.
//
// The broker URL is resolved here from the bootstrap and baked into BOTH sidecars
// as --broker-url, so neither reads the bootstrap file at all: it avoids
// re-touching the one-time enrolment ticket the bootstrap also carries, and
// removes any dependency on the sidecar's uid/CWD matching the bootstrap's perms
// (the sidecars run as the agent user; the bootstrap is 0600). No-op when the
// bootstrap is absent or carries no broker URL: a non-brokered agent has no broker
// to proxy to or renew against. The renew loop self-heals transient errors
// (logged, loop continues); the gateway is fail-fast — restart:on-failure
// relaunches it on a crash. The gateway is emitted first so it is up before renew.
//
// Tamper window: this file sits at $HOME/.scion/ under the agent-writable
// /home/scion bind-mount, so a compromised agent could rewrite it. That grants
// no escalation — scion launches the sidecar at the agent's OWN uid (nothing it
// couldn't already exec), and boot runs inside the pre-start hook, which scion
// runs BEFORE it reads scion-services.yaml, so this write always wins over any
// pre-seeded value before the spec is consumed.
func WriteSidecarSpecs(c SidecarConfig) error {
	bs, err := LoadBootstrap(c.BootstrapPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // no bootstrap deposited — non-brokered agent, emit no sidecar
		}
		return fmt.Errorf("load bootstrap: %w", err)
	}
	if bs.BrokerURL == "" {
		return nil // brokerless bootstrap — nothing to renew against
	}
	gatewayCommand := []string{
		"lever-agent", "gateway",
		"--id-dir", c.IDDir,
		"--broker-url", bs.BrokerURL,
		"--listen", LocalGatewayAddr,
	}
	renewCommand := []string{
		"lever-agent", "renew", "--loop",
		"--id-dir", c.IDDir,
		"--broker-url", bs.BrokerURL,
		"--llm-auth", c.LLMAuth,
		"--settings", c.SettingsPath,
	}
	specs := []sidecarSpec{
		{Name: "lever-gateway", Command: gatewayCommand, Restart: "on-failure"},
		{Name: "lever-renew", Command: renewCommand, Restart: "on-failure"},
	}
	b, err := yaml.Marshal(specs)
	if err != nil {
		return err
	}
	dir := filepath.Join(c.HomeDir, layout.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("scion dir %s: %w", dir, err)
	}
	return os.WriteFile(filepath.Join(dir, "scion-services.yaml"), b, 0o644)
}
