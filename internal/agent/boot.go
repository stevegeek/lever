package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/wire"
)

// Bootstrap is the material the host or broker deposits for an agent to enrol.
type Bootstrap = wire.Bootstrap

// Claude is the envelope's per-agent Claude Code config (wire.Claude).
type Claude = wire.Claude

// NormalizeBrokerURL strips trailing slashes so callers can append a route.
func NormalizeBrokerURL(u string) string {
	return strings.TrimRight(u, "/")
}

// LoadBootstrap reads the deposited bootstrap.json and normalizes BrokerURL.
// For the manager it sits in the agent-writable tree; for a worker it is
// /run/lever/bootstrap.json, mounted read-only into that container alone. boot
// may read it as root (see errRefusedPath), so the read is confined two levels
// up and a symlink or non-regular file at the directory or at bootstrap.json
// is refused. An absent file still reports fs.ErrNotExist.
func LoadBootstrap(path string) (Bootstrap, error) {
	root, rel := splitAbove(path, 2)
	r, err := os.OpenRoot(root)
	if err != nil {
		return Bootstrap{}, fmt.Errorf("agent: read bootstrap: %w", err)
	}
	defer r.Close()
	if err := checkedFile(r, rel); err != nil {
		return Bootstrap{}, err
	}
	b, err := r.ReadFile(rel)
	if err != nil {
		return Bootstrap{}, fmt.Errorf("agent: read bootstrap: %w", err)
	}
	var bs Bootstrap
	if err := json.Unmarshal(b, &bs); err != nil {
		return Bootstrap{}, fmt.Errorf("agent: parse bootstrap: %w", err)
	}
	bs.BrokerURL = NormalizeBrokerURL(bs.BrokerURL)
	return bs, nil
}

// BootConfig drives Boot.
type BootConfig struct {
	BootstrapPath string
	IDDir         string
	// BrokerTools are the broker tool names to register with claude. When
	// empty and DiscoverTools is set, the list is fetched from the broker
	// after enrolment, failing closed.
	BrokerTools   []string
	DiscoverTools bool
	// SettingsPath is the claude settings.json whose env block receives the
	// harness overlay (WriteSettingsEnv). Empty skips the write (enrol-only).
	SettingsPath string
	// ManagedSettingsPath receives the envelope's claude block
	// (WriteManagedSettings). Empty skips the write.
	ManagedSettingsPath string
	// Log reports a boot decision the operator should see in the agent's
	// pre-start output. nil discards it.
	Log func(format string, args ...any)
	// LLMAuth selects the LLM-auth mode ("api-key" | "subscription" | "").
	// Only "api-key" writes ANTHROPIC_AUTH_TOKEN and ANTHROPIC_BASE_URL into
	// the overlay.
	LLMAuth string
	// MCPAdd registers one MCP server with claude. nil skips registration.
	MCPAdd func(name string, argv ...string) error
}

// Boot enrols the agent (idempotently) and configures the harness: identity,
// settings overlays and MCP server registrations.
func Boot(ctx context.Context, c BootConfig) error {
	// Load bootstrap before the enrol check so BrokerURL is known on resume
	// too. An absent bootstrap leaves brokerURL empty and no broker tools are
	// registered.
	var brokerURL string
	var claude *Claude
	bs, bsErr := LoadBootstrap(c.BootstrapPath)
	if bsErr == nil {
		brokerURL = bs.BrokerURL
		// The manager's envelope is in a tree the manager writes, so validate
		// again. An invalid block is dropped and logged: the settings are
		// optional and must not stop the agent.
		if err := bs.Claude.Validate(); err != nil {
			if c.Log != nil {
				c.Log("bootstrap claude block refused, not delivered: %v", err)
			}
		} else {
			claude = bs.Claude
		}
	}

	// A valid existing cert means already enrolled.
	id, ok := LoadIdentity(c.IDDir)
	if !ok || !ValidCert(id.CertPEM, time.Now()) {
		if bsErr != nil {
			return bsErr
		}
		var err error
		id, err = Enrol(ctx, bs.BrokerURL, []byte(bs.BrokerCA), bs.Ticket, bs.AgentCN)
		if err != nil {
			return err
		}
		if err := id.Write(c.IDDir); err != nil {
			return err
		}
	}

	// Boot-time broker calls use the direct mTLS client: the gateway sidecar is
	// not up yet during pre-start.
	client, err := id.Client()
	if err != nil {
		return fmt.Errorf("agent: boot: build mTLS client: %w", err)
	}

	brokerTools := c.BrokerTools
	if len(brokerTools) == 0 && c.DiscoverTools && brokerURL != "" {
		brokerTools, err = ListTools(ctx, brokerURL, client)
		if err != nil {
			return err
		}
	}

	overlay := HarnessEnvOverlay()
	// Fail closed: a partial overlay without a valid token is worse than a
	// failed boot.
	if c.LLMAuth == LLMAuthAPIKey {
		if err := RefreshLLMToken(ctx, brokerURL, id, overlay); err != nil {
			return fmt.Errorf("agent boot: %w", err)
		}
	}
	if err := WriteSettingsEnv(c.SettingsPath, overlay); err != nil {
		return err
	}
	if bsErr != nil && c.ManagedSettingsPath != "" && c.Log != nil {
		// No envelope means no claude block: say why lever's values go.
		c.Log("no readable bootstrap envelope (%v); removing lever's claude settings from %s, if any (the agent-view switch is still written)", bsErr, c.ManagedSettingsPath)
	}
	// A failed write must not stop the agent: under rootless podman the hook
	// runs as the agent user, and an image without a writable
	// /etc/claude-code cannot take the file. It is logged instead.
	if err := WriteManagedSettings(c.ManagedSettingsPath, claude); err != nil && c.Log != nil {
		if claude.IsZero() {
			c.Log("agent-view switch not written (%v); background sessions stay possible", err)
		} else {
			c.Log("claude settings not delivered, nor the agent-view switch (background sessions stay possible): %v", err)
		}
	}
	if c.MCPAdd == nil {
		return nil
	}
	if err := c.MCPAdd("lever-capability", "lever-agent", "serve-capability"); err != nil {
		return err
	}
	// Broker tools are reached through the loopback gateway, which presents
	// the rotating agent leaf on Claude's behalf.
	if brokerURL == "" {
		return nil
	}
	for _, tool := range brokerTools {
		if err := c.MCPAdd(tool, "--transport", "http", LocalGatewayURL+"/mcp/"+tool+"/"); err != nil {
			return err
		}
	}
	return nil
}
