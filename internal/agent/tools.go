package agent

import (
	"context"
	"fmt"
	"net/http"

	"github.com/stevegeek/lever/internal/httpjson"
)

// ListTools fetches the broker's registered tool names over mTLS so the agent
// can register each tool's /mcp/<tool>/ gateway with claude at boot.
func ListTools(ctx context.Context, brokerURL string, client *http.Client) ([]string, error) {
	var out struct {
		Tools []string `json:"tools"`
	}
	if err := httpjson.Get(ctx, client, brokerURL+"/tools", &out); err != nil {
		return nil, fmt.Errorf("agent: list tools: %w", err)
	}
	return out.Tools, nil
}
