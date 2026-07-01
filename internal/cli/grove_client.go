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

// postGrove POSTs body as JSON to baseURL+endpoint using client, decoding the
// grove response. Split out for unit-testing without mTLS.
func postGrove(ctx context.Context, client *http.Client, baseURL, endpoint string, body any) (groveResult, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return groveResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+endpoint, bytes.NewReader(raw))
	if err != nil {
		return groveResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return groveResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return groveResult{}, fmt.Errorf("broker %s returned %d", endpoint, resp.StatusCode)
	}
	var res groveResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return groveResult{}, err
	}
	return res, nil
}

// groveCall builds the manager's mTLS client from its bootstrap + identity and
// POSTs endpoint. This is the production entry the agent subcommands use.
func groveCall(ctx context.Context, endpoint string, body any) (groveResult, error) {
	bs, err := agent.LoadBootstrap(managerBootstrapPath)
	if err != nil {
		return groveResult{}, fmt.Errorf("manager bootstrap: %w", err)
	}
	id, ok := agent.LoadIdentity(managerIDDir)
	if !ok {
		return groveResult{}, fmt.Errorf("manager identity not found in %s", managerIDDir)
	}
	client, err := id.Client()
	if err != nil {
		return groveResult{}, fmt.Errorf("manager mTLS client: %w", err)
	}
	return postGrove(ctx, client, bs.BrokerURL, endpoint, body)
}
