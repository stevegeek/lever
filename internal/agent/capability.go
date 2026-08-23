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
// gateway at /mcp/<tool>/. The capability token MUST travel in
// params.arguments._capability (NOT a header): the gateway reads the token
// exclusively from that field and actively scrubs all inbound X-Lever-* headers.
// The tool name is encoded in the URL path; op maps to params.name (the operation
// within that tool); extra constraints are merged into arguments.
//
// Unlike Request (which swallows a non-200 body into the error), Call returns the
// raw response body, the HTTP status, AND the error separately, because the caller
// prints the body to the user BEFORE surfacing the non-200 error — the acceptance
// harness's deny checks rely on both the printed output and the non-zero exit. A
// non-200 yields body+status+`call: status %d`; transport or read failures yield
// an empty body, zero status, and the wrapped error.
func Call(ctx context.Context, brokerURL string, client *http.Client, tool, op, token string, constraints map[string]string) (string, int, error) {
	body := buildToolCallBody(op, token, constraints)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, brokerURL+"/mcp/"+tool+"/", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("call: %w", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return "", 0, fmt.Errorf("call: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return buf.String(), resp.StatusCode, fmt.Errorf("call: status %d", resp.StatusCode)
	}
	return buf.String(), resp.StatusCode, nil
}

// buildToolCallBody constructs the JSON-RPC 2.0 body for a tools/call request to
// the capability gateway. The token is placed in arguments._capability as required
// by the gateway contract (internal/mcp.ToolsCall). The tool's URL
// path carries the tool name; op maps to params.name (the operation within that
// tool). Extra key=value constraints from the CLI are merged into arguments.
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
