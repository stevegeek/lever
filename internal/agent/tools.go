package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ListTools fetches the broker's registered tool names over mTLS so the agent
// can register each tool's /mcp/<tool>/ gateway with claude at boot.
func ListTools(ctx context.Context, brokerURL string, client *http.Client) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, brokerURL+"/tools", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent: list tools: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent: list tools status %d", resp.StatusCode)
	}
	var out struct {
		Tools []string `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("agent: list tools decode: %w", err)
	}
	return out.Tools, nil
}
