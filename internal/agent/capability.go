package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
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
	cr, err := postJSON[struct {
		Token string `json:"token"`
	}](ctx, client, brokerURL+"/request",
		map[string]any{"tool": tool, "op": op, "bound_to": boundTo, "constraints": constraints},
		0, "agent: request", true)
	if err != nil {
		return "", err
	}
	return cr.Token, nil
}
