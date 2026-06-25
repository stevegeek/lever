package agent

import (
	"context"
	"encoding/json"
	"fmt"
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
	BrokerTools     []string // tool names → claude mcp add /mcp/<name>/
	Now             time.Time
	MCPAdd          func(name string, argv ...string) error
	WriteEnvOverlay func(map[string]string) error
}

// Boot enrols the agent (idempotently) and configures the harness: writes the
// identity, the env overlay (CLAUDE_CODE_CLIENT_CERT/_KEY + NODE_EXTRA_CA_CERTS),
// and registers the capability MCP server + each broker /mcp/<tool>/ via MCPAdd.
func Boot(ctx context.Context, c BootConfig) error {
	if c.Now.IsZero() {
		c.Now = time.Now()
	}
	// Idempotent: a valid existing cert means we already enrolled (resume/restart).
	id, ok := LoadIdentity(c.IDDir)
	if !ok || !ValidCert(id.CertPEM, c.Now) {
		bs, err := LoadBootstrap(c.BootstrapPath)
		if err != nil {
			return err
		}
		id, err = Enrol(ctx, bs.BrokerURL, []byte(bs.BrokerCA), bs.Ticket, bs.AgentCN)
		if err != nil {
			return err
		}
		if err := id.Write(c.IDDir); err != nil {
			return err
		}
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
		if err := c.MCPAdd("lever-capability", "lever-agent", "serve-capability"); err != nil {
			return err
		}
		for _, tool := range c.BrokerTools {
			if err := c.MCPAdd(tool, "--", "/mcp/"+tool+"/"); err != nil {
				return err
			}
		}
	}
	return nil
}
