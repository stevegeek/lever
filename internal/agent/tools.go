package agent

import (
	"context"
	"net/http"
)

// ListTools fetches the broker's registered tool names over mTLS so the agent
// can register each tool's /mcp/<tool>/ gateway with claude at boot. The decode
// is bounded to 1 MiB (the only broker helper with a body limit today).
func ListTools(ctx context.Context, brokerURL string, client *http.Client) ([]string, error) {
	out, err := getJSON[struct {
		Tools []string `json:"tools"`
	}](ctx, client, brokerURL+"/tools", 1<<20, "agent: list tools")
	if err != nil {
		return nil, err
	}
	return out.Tools, nil
}
