package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
