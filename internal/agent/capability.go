package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Client builds an mTLS http.Client presenting this identity's cert and trusting
// its CA. The cert is baked in STATICALLY (loaded once), so this is for
// SHORT-LIVED, one-shot calls only (boot-time tool discovery, the `request`/
// `delegate`/`call`/`provision` CLIs). A LONG-LIVED daemon must NOT use this — it
// would freeze the boot leaf and keep presenting it after the 24h TTL despite
// lever-renew rotating the on-disk leaf; use NewReloadingClient instead (see
// gateway.go), which re-reads per handshake.
func (id Identity) Client() (*http.Client, error) {
	cert, err := tls.X509KeyPair(id.CertPEM, id.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("agent: client cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(id.CAPEM) {
		return nil, fmt.Errorf("agent: bad CA PEM")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{cert}, RootCAs: pool,
	}}}, nil
}

// Request mints a capability token via the broker's /request endpoint. boundTo is
// the caller (self-obtain) or another agent (delegation). Returns the base64url token.
func Request(ctx context.Context, brokerURL string, client *http.Client, tool, op, boundTo string, constraints map[string]string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"tool": tool, "op": op, "bound_to": boundTo, "constraints": constraints,
	})
	if err != nil {
		return "", fmt.Errorf("agent: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", brokerURL+"/request", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("agent: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("agent: request status %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	var cr struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", fmt.Errorf("agent: request decode: %w", err)
	}
	return cr.Token, nil
}
