package agent

import (
	"context"
	"fmt"

	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/wire"
)

// Renew rotates the agent's keypair and re-issues its cert under the broker's
// authenticated CN (the CSR CN is ignored by /renew). The new private key never
// leaves this process.
func Renew(ctx context.Context, brokerURL string, id Identity) (Identity, error) {
	cn := "renew" // CN ignored by /renew; broker re-issues under the authenticated CN.
	csrPEM, keyPEM, err := GenerateCSR(cn)
	if err != nil {
		return Identity{}, err
	}
	client, err := id.Client()
	if err != nil {
		return Identity{}, err
	}
	var rr wire.RenewResponse
	if err := httpjson.Post(ctx, client, brokerURL+wire.PathRenew, wire.RenewRequest{CSR: string(csrPEM)}, &rr); err != nil {
		return Identity{}, fmt.Errorf("agent: renew: %w", err)
	}
	return Identity{CertPEM: []byte(rr.Cert), KeyPEM: keyPEM, CAPEM: id.CAPEM}, nil
}

// RenewConfig drives RenewOnce.
type RenewConfig struct {
	Identity  Identity // current identity (the caller loaded it from IDDir)
	IDDir     string   // where the rotated identity is written back
	BrokerURL string   // already resolved (flag or bootstrap)
	// LLMAuth "api-key" with a non-empty SettingsPath also refreshes
	// ANTHROPIC_AUTH_TOKEN after the cert is renewed and rewrites the claude
	// settings.json env block at SettingsPath.
	LLMAuth      string
	SettingsPath string
}

// RenewOnce performs a single renewal cycle: Renew c.Identity, write the
// renewed identity back to c.IDDir, then (api-key mode) refresh the LLM
// capability token and rewrite the settings.json env block so the next claude
// launch picks up the fresh token.
func RenewOnce(ctx context.Context, c RenewConfig) error {
	renewed, err := Renew(ctx, c.BrokerURL, c.Identity)
	if err != nil {
		return err
	}
	if err := renewed.Write(c.IDDir); err != nil {
		return err
	}
	if c.LLMAuth != LLMAuthAPIKey || c.SettingsPath == "" {
		return nil
	}
	overlay := HarnessEnvOverlay()
	if err := RefreshLLMToken(ctx, c.BrokerURL, renewed, overlay); err != nil {
		return err
	}
	return WriteSettingsEnv(c.SettingsPath, overlay)
}
