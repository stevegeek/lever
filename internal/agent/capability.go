package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/wire"
)

// Client builds an mTLS http.Client presenting this identity's cert and trusting
// its CA. The cert is loaded once, so this is for short-lived calls only; a
// long-lived daemon must use NewReloadingClient, or it keeps presenting the
// boot leaf after renewal and expiry.
func (id Identity) Client() (*http.Client, error) {
	cert, err := tls.X509KeyPair(id.CertPEM, id.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("agent: client cert: %w", err)
	}
	pool, err := caPool(id.CAPEM)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{cert}, RootCAs: pool,
	}}}, nil
}

// Request mints a capability token via the broker's /request endpoint. boundTo is
// the caller (self-obtain) or another agent (delegation). Returns the base64url token.
func Request(ctx context.Context, brokerURL string, client *http.Client, tool, op, boundTo string, constraints map[string]string) (string, error) {
	var cr wire.CapResponse
	in := wire.CapRequest{Tool: tool, Op: op, BoundTo: boundTo, Constraints: constraints}
	if err := httpjson.Post(ctx, client, brokerURL+wire.PathRequest, in, &cr); err != nil {
		return "", fmt.Errorf("agent: request: %w", err)
	}
	return cr.Token, nil
}

// Call exercises a capability by POSTing a JSON-RPC 2.0 tools/call to the broker
// gateway at /mcp/<tool>/. The token must travel in
// params.arguments._capability, not a header: the gateway reads it only from
// there and scrubs inbound X-Lever-* headers.
//
// A non-200 returns both the body and a wrapped *httpjson.StatusError, because
// the caller prints the body before surfacing the error. Transport or read
// failures return an empty body.
func Call(ctx context.Context, brokerURL string, client *http.Client, tool, op, token string, constraints map[string]string) (string, error) {
	body := buildToolCallBody(op, token, constraints)
	url := brokerURL + "/mcp/" + tool + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("call: %w", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return "", fmt.Errorf("call: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Body deliberately left out of the StatusError: the caller prints it.
		return buf.String(), fmt.Errorf("call: %w", &httpjson.StatusError{Method: http.MethodPost, URL: url, Code: resp.StatusCode})
	}
	return buf.String(), nil
}

func buildToolCallBody(op, token string, args map[string]string) []byte {
	arguments := make(map[string]any, len(args)+1)
	for k, v := range args {
		arguments[k] = v
	}
	arguments["_capability"] = token
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      op,
			"arguments": arguments,
		},
	}
	out, _ := json.Marshal(body)
	return out
}
