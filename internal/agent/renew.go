package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
	body, _ := json.Marshal(map[string]string{"csr": string(csrPEM)})
	req, err := http.NewRequestWithContext(ctx, "POST", brokerURL+"/renew", bytes.NewReader(body))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("agent: renew: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("agent: renew status %d", resp.StatusCode)
	}
	var rr struct {
		Cert string `json:"cert"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return Identity{}, fmt.Errorf("agent: renew decode: %w", err)
	}
	return Identity{CertPEM: []byte(rr.Cert), KeyPEM: keyPEM, CAPEM: id.CAPEM}, nil
}

// RefreshLLMToken obtains a fresh capability(llm) token from the broker using
// the enrolled mTLS identity and merges ANTHROPIC_AUTH_TOKEN + ANTHROPIC_BASE_URL
// into the provided overlay map. The token is returned verbatim from the broker
// (already base64url-encoded) — do NOT re-encode it. Called by the renewal sidecar
// on each cert renewal cycle when LLM auth mode is "api-key".
//
// Fail closed: the overlay map is only mutated after a successful token acquisition.
func RefreshLLMToken(
	ctx context.Context,
	brokerURL string,
	id Identity,
	cn string,
	requestFn func(ctx context.Context, brokerURL string, client *http.Client, cn string) (string, error),
	overlay map[string]string,
) error {
	client, err := id.Client()
	if err != nil {
		return fmt.Errorf("agent: refresh llm token: build mTLS client: %w", err)
	}
	tok, err := requestFn(ctx, brokerURL, client, cn)
	if err != nil {
		return fmt.Errorf("agent: refresh llm token: obtain: %w", err)
	}
	overlay["ANTHROPIC_AUTH_TOKEN"] = tok
	overlay["ANTHROPIC_BASE_URL"] = strings.TrimRight(brokerURL, "/") + "/llm"
	return nil
}
