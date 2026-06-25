package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Provision mints a one-use enrolment ticket for a grove via the broker's
// /provision endpoint over the caller's mTLS identity. /provision is
// manager-CN-gated by the broker, so client must present the manager identity.
func Provision(ctx context.Context, brokerURL string, client *http.Client, grove string) (string, error) {
	body, err := json.Marshal(map[string]string{"grove": grove})
	if err != nil {
		return "", fmt.Errorf("agent: marshal provision: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", brokerURL+"/provision", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("agent: provision: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("agent: provision status %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	var pr struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return "", fmt.Errorf("agent: provision decode: %w", err)
	}
	return pr.Ticket, nil
}

// BootstrapFor composes the grove Bootstrap a freshly-provisioned grove enrols with.
func BootstrapFor(grove, ticket, brokerCA, brokerURL string) Bootstrap {
	return Bootstrap{Ticket: ticket, BrokerCA: brokerCA, BrokerURL: brokerURL, AgentCN: grove}
}
