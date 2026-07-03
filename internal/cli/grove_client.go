package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/lever-to/lever/internal/agent"
	"github.com/lever-to/lever/internal/scion"
)

type groveResult struct {
	Grove  string        `json:"grove"`
	Phase  string        `json:"phase"`
	Agents []scion.Agent `json:"agents"`
}

// postBroker POSTs body as JSON to baseURL+endpoint using client, decoding the
// response into T. Split out for unit-testing without mTLS. Generic sibling of
// the old grove-only postGrove; postGrove/groveCall now specialize it.
func postBroker[T any](ctx context.Context, client *http.Client, baseURL, endpoint string, body any) (T, error) {
	var zero T
	raw, err := json.Marshal(body)
	if err != nil {
		return zero, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+endpoint, bytes.NewReader(raw))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("broker %s returned %d", endpoint, resp.StatusCode)
	}
	var res T
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return zero, err
	}
	return res, nil
}

// postGrove is postBroker specialized to the grove-command response shape.
// Kept as a named function (rather than inlining postBroker[groveResult] at
// call sites) so existing callers/tests are unaffected.
func postGrove(ctx context.Context, client *http.Client, baseURL, endpoint string, body any) (groveResult, error) {
	return postBroker[groveResult](ctx, client, baseURL, endpoint, body)
}

// brokerCall builds the manager's mTLS client from its bootstrap + identity and
// POSTs endpoint, decoding into T. This is the production entry both the agent
// (grove) and msg/watch subcommands use — the bootstrap/identity paths are
// agent-generic (groves get their own bootstrap at the same in-container path,
// so the same binary works for manager AND groves).
//
// brokerCall is generic, so it cannot be assigned directly to a package-level
// seam var (`var xCallFn = brokerCall` doesn't type-check without explicit
// instantiation). Each call site instead gets a small concrete wrapper
// (groveCall, msgCall) that instantiates brokerCall for its response type;
// the wrapper is what the test seam (groveCallFn, msgCallFn) points at.
func brokerCall[T any](ctx context.Context, endpoint string, body any) (T, error) {
	var zero T
	bs, err := agent.LoadBootstrap(managerBootstrapPath)
	if err != nil {
		return zero, fmt.Errorf("manager bootstrap: %w", err)
	}
	id, ok := agent.LoadIdentity(managerIDDir)
	if !ok {
		return zero, fmt.Errorf("manager identity not found in %s", managerIDDir)
	}
	client, err := id.Client()
	if err != nil {
		return zero, fmt.Errorf("manager mTLS client: %w", err)
	}
	return postBroker[T](ctx, client, bs.BrokerURL, endpoint, body)
}

// groveCall is brokerCall specialized to the grove-command response shape.
// This is the production entry the agent subcommands use (via groveCallFn).
func groveCall(ctx context.Context, endpoint string, body any) (groveResult, error) {
	return brokerCall[groveResult](ctx, endpoint, body)
}
