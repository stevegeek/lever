package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/lever-to/lever/internal/cap/token"
)

// Client builds an mTLS http.Client presenting this identity's cert and trusting its CA.
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
	body, _ := json.Marshal(map[string]any{
		"tool": tool, "op": op, "bound_to": boundTo, "constraints": constraints,
	})
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
		return "", fmt.Errorf("agent: request status %d", resp.StatusCode)
	}
	var cr struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", fmt.Errorf("agent: request decode: %w", err)
	}
	return cr.Token, nil
}

// Attenuate appends narrowing constraints to a base64url token, offline (no broker).
func Attenuate(tokenB64 string, constraints map[string]string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tokenB64)
	if err != nil {
		return "", fmt.Errorf("agent: decode token: %w", err)
	}
	extra := make([]token.Constraint, 0, len(constraints))
	for k, v := range constraints {
		extra = append(extra, token.Constraint{Key: k, Value: v})
	}
	narrowed, err := token.Attenuate(raw, extra)
	if err != nil {
		return "", fmt.Errorf("agent: attenuate: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(narrowed), nil
}
