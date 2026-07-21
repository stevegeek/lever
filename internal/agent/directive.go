package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// directivePost posts {"id":id} to a directive route over the agent's own
// mTLS channel and returns the raw JSON body on 200. Non-200 bodies are
// deliberately terse (the broker's opaque contract) — surface them as-is.
func directivePost(ctx context.Context, brokerURL string, client *http.Client, route, id string) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]string{"id": id})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, brokerURL+route, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", bytes.TrimSpace(b))
	}
	return json.RawMessage(b), nil
}

// DirectiveConsume atomically consumes an operator directive over the
// agent's mTLS channel. The returned JSON is the ONLY authoritative body.
func DirectiveConsume(ctx context.Context, brokerURL string, client *http.Client, id string) (json.RawMessage, error) {
	return directivePost(ctx, brokerURL, client, "/directive/consume", id)
}

// DirectiveCheck reads a directive's status (target-gated, read-only).
func DirectiveCheck(ctx context.Context, brokerURL string, client *http.Client, id string) (json.RawMessage, error) {
	return directivePost(ctx, brokerURL, client, "/directive/check", id)
}
