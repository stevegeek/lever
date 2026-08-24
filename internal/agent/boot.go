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

// Bootstrap is the material the host/manager deposits for an agent to enrol. It
// aliases wire.Bootstrap — the single, cross-package declaration of the
// enrolment envelope (the host stages it via wire.Stage; agent reads it via
// LoadBootstrap). The alias keeps agent.Bootstrap as the name agent-side code
// uses while the fields/tags live in exactly one place.
type Bootstrap = wire.Bootstrap

// NormalizeBrokerURL strips trailing slashes so callers can append "/enrol",
// "/request", ... without producing "//enrol".
func NormalizeBrokerURL(u string) string {
	return strings.TrimRight(u, "/")
}

// LoadBootstrap reads the deposited bootstrap.json. BrokerURL is normalized via
// NormalizeBrokerURL.
func LoadBootstrap(path string) (Bootstrap, error) {
	b, err := os.ReadFile(path)
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

// BootConfig drives Boot. MCPAdd is the one injected seam: it execs `claude mcp
// add` in production and is recorded by tests, which otherwise run Boot against
// a real (test) broker.
type BootConfig struct {
	BootstrapPath string
	IDDir         string
	// BrokerTools are the broker tool names to register with claude (each via
	// `claude mcp add --transport http <gateway>/mcp/<name>/`). When empty and
	// DiscoverTools is set, the list is fetched from the broker's /tools
	// endpoint after enrolment (fail-closed: a booting agent that cannot learn
	// its tools is a real failure, not a tolerable degraded state).
	BrokerTools   []string
	DiscoverTools bool
	// SettingsPath is the claude settings.json whose env block receives the
	// harness overlay (WriteSettingsEnv). Empty skips the write (enrol-only).
	SettingsPath string
	// LLMAuth selects the LLM-auth mode ("api-key" | "subscription" | "").
	// When "api-key", Boot obtains a capability(llm) token and writes
	// ANTHROPIC_AUTH_TOKEN + ANTHROPIC_BASE_URL into the overlay. Any other
	// value leaves those keys absent.
	LLMAuth string
	// MCPAdd registers one MCP server with claude. nil skips registration (the
	// enrol-only acceptance gate has no `claude` binary).
	MCPAdd func(name string, argv ...string) error
}

// Boot enrols the agent (idempotently) and configures the harness: writes the
// identity, the claude settings.json env overlay, and registers the capability
// MCP server + each broker /mcp/<tool>/ (via the loopback gateway) with MCPAdd.
func Boot(ctx context.Context, c BootConfig) error {
	// Load bootstrap early so BrokerURL is available on both the enrol AND
	// skip-enrol (resume/restart) paths. Reading the file is cheap and idempotent;
	// the ticket inside is only redeemed during enrol. If bootstrap is absent
	// (no broker configured) we tolerate it by leaving brokerURL empty — the
	// broker-tool registration loop will simply register nothing.
	var brokerURL string
	bs, bsErr := LoadBootstrap(c.BootstrapPath)
	if bsErr == nil {
		brokerURL = bs.BrokerURL
	}

	// Idempotent: a valid existing cert means we already enrolled (resume/restart).
	id, ok := LoadIdentity(c.IDDir)
	if !ok || !ValidCert(id.CertPEM, time.Now()) {
		if bsErr != nil {
			// Bootstrap required for first enrol; surface the error.
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

	// Boot-time broker calls (discovery, llm token) use the direct mTLS client:
	// the gateway sidecar is not up yet during pre-start. Only the values written
	// into Claude's config point at the loopback gateway.
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
	// api-key mode: obtain a capability(llm) token and inject the Anthropic env vars.
	// Fail closed: a partial overlay without a valid token is worse than a failed boot.
	if c.LLMAuth == LLMAuthAPIKey {
		if err := RefreshLLMToken(ctx, brokerURL, id, overlay); err != nil {
			return fmt.Errorf("agent boot: %w", err)
		}
	}
	if err := WriteSettingsEnv(c.SettingsPath, overlay); err != nil {
		return err
	}
	if c.MCPAdd == nil {
		return nil
	}
	// Capability server stays as stdio (lever-agent subprocess).
	if err := c.MCPAdd("lever-capability", "lever-agent", "serve-capability"); err != nil {
		return err
	}
	// Broker tools are HTTP MCP servers reached through the loopback gateway,
	// which presents the rotating agent leaf on Claude's behalf. If brokerURL is
	// empty (no bootstrap configured) there are no broker tools to route.
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
