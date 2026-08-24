package agent

import (
	"context"
	"fmt"
	"net/http"

	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/wire"
)

// Provision mints a one-use enrolment ticket for a worker via the broker's
// /provision endpoint over the caller's mTLS identity. /provision is
// manager-CN-gated by the broker, so client must present the manager identity.
func Provision(ctx context.Context, brokerURL string, client *http.Client, worker string) (string, error) {
	var pr wire.ProvisionResponse
	if err := httpjson.Post(ctx, client, brokerURL+wire.PathProvision, wire.ProvisionRequest{Worker: worker}, &pr); err != nil {
		return "", fmt.Errorf("agent: provision: %w", err)
	}
	return pr.Ticket, nil
}
