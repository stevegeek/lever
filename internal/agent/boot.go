package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Bootstrap is the material the host/manager deposits for an agent to enrol.
type Bootstrap struct {
	Ticket    string `json:"ticket"`
	BrokerCA  string `json:"broker_ca"`
	BrokerURL string `json:"broker_url"`
	AgentCN   string `json:"agent_cn"`
}

// LoadBootstrap reads the deposited bootstrap.json.
func LoadBootstrap(path string) (Bootstrap, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Bootstrap{}, fmt.Errorf("agent: read bootstrap: %w", err)
	}
	var bs Bootstrap
	if err := json.Unmarshal(b, &bs); err != nil {
		return Bootstrap{}, fmt.Errorf("agent: parse bootstrap: %w", err)
	}
	return bs, nil
}

// BootConfig drives Boot. MCPAdd + WriteEnvOverlay are injected so tests assert
// the configuration without a real `claude` binary or the scion overlay file.
type BootConfig struct {
	BootstrapPath   string
	IDDir           string
	BrokerTools     []string // tool names → claude mcp add --transport http <broker-url>/mcp/<name>/
	Now             time.Time
	MCPAdd          func(name string, argv ...string) error
	WriteEnvOverlay func(map[string]string) error
	// ListTools, when non-nil and BrokerTools is empty, is called after enrolment
	// to auto-discover registered tool names from the broker. Injected so tests
	// can stub it; production sets it to agent.ListTools.
	ListTools func(ctx context.Context, brokerURL string, client *http.Client) ([]string, error)
}

// Boot enrols the agent (idempotently) and configures the harness: writes the
// identity, the env overlay (CLAUDE_CODE_CLIENT_CERT/_KEY + NODE_EXTRA_CA_CERTS),
// and registers the capability MCP server + each broker /mcp/<tool>/ via MCPAdd.
func Boot(ctx context.Context, c BootConfig) error {
	if c.Now.IsZero() {
		c.Now = time.Now()
	}

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
	if !ok || !ValidCert(id.CertPEM, c.Now) {
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

	// Resolve broker tools: use explicit list when provided; otherwise auto-discover
	// via the broker's /tools endpoint (fail-closed — a booting agent that can't
	// learn its tools is a real failure, not a tolerable degraded state).
	brokerTools := c.BrokerTools
	if len(brokerTools) == 0 && c.ListTools != nil && brokerURL != "" {
		client, err := id.Client()
		if err != nil {
			return fmt.Errorf("agent: boot: build mTLS client for tool discovery: %w", err)
		}
		discovered, err := c.ListTools(ctx, brokerURL, client)
		if err != nil {
			return err
		}
		brokerTools = discovered
	}

	// Env overlay points the harness at the identity files (paths only, never key bytes).
	overlay := map[string]string{
		"CLAUDE_CODE_CLIENT_CERT": filepath.Join(c.IDDir, "agent.crt"),
		"CLAUDE_CODE_CLIENT_KEY":  filepath.Join(c.IDDir, "agent.key"),
		"NODE_EXTRA_CA_CERTS":     filepath.Join(c.IDDir, "ca.crt"),
	}
	if c.WriteEnvOverlay != nil {
		if err := c.WriteEnvOverlay(overlay); err != nil {
			return err
		}
	}
	if c.MCPAdd != nil {
		// Capability server stays as stdio (lever-agent subprocess).
		if err := c.MCPAdd("lever-capability", "lever-agent", "serve-capability"); err != nil {
			return err
		}
		// Broker tools are HTTP MCP servers at the broker. The mTLS client cert
		// for these calls is wired in by the env overlay above (paths only).
		// If brokerURL is empty (no bootstrap configured), skip registration.
		for _, tool := range brokerTools {
			if brokerURL == "" {
				continue
			}
			if err := c.MCPAdd(tool, "--transport", "http", brokerURL+"/mcp/"+tool+"/"); err != nil {
				return err
			}
		}
	}
	return nil
}
