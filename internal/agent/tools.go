package agent

import (
	"context"
	"fmt"
	"net/http"

	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/wire"
)

// ListTools fetches the broker's registered tool names over mTLS so the agent
// can register each tool's /mcp/<tool>/ gateway with claude at boot.
func ListTools(ctx context.Context, brokerURL string, client *http.Client) ([]string, error) {
	var out wire.ToolsResponse
	if err := httpjson.Get(ctx, client, brokerURL+wire.PathTools, &out); err != nil {
		return nil, fmt.Errorf("agent: list tools: %w", err)
	}
	return out.Tools, nil
}
