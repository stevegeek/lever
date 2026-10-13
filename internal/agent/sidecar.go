package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stevegeek/lever/internal/scion/layout"
	"gopkg.in/yaml.v3"
)

// sidecarSpec mirrors the subset of scion's api.ServiceSpec lever emits; the
// yaml keys must match.
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

// WriteSidecarSpecs emits $HOME/.scion/scion-services.yaml describing the
// lever-gateway and lever-renew sidecars, which scion launches after the
// pre-start hooks as the agent user with the container env. scion does not
// set the sidecar's working directory, so every path is absolute.
//
// The broker URL is baked into both sidecars so neither reads the bootstrap,
// which carries the one-time enrolment ticket. No-op when the bootstrap is
// absent or has no broker URL. The gateway is emitted first so it is up
// before renew.
//
// The file is agent-writable, but rewriting it grants nothing: scion launches
// the sidecars at the agent's own uid, and reads the file only after the
// pre-start hook, so this write wins. What the agent must not get is this
// write redirected: boot may run as root, so the write is confined to
// c.HomeDir and a symlink at .scion or at the yaml is refused
// (errRefusedPath).
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
	home := c.HomeDir
	if home == "" {
		home = "."
	}
	r, err := os.OpenRoot(home)
	if err != nil {
		return fmt.Errorf("home %s: %w", home, err)
	}
	defer r.Close()
	spec := filepath.Join(layout.Dir, "scion-services.yaml")
	if err := checkedFile(r, spec); err != nil {
		return err
	}
	if err := r.MkdirAll(layout.Dir, 0o755); err != nil {
		return fmt.Errorf("scion dir %s: %w", filepath.Join(home, layout.Dir), err)
	}
	return r.WriteFile(spec, b, 0o644)
}
