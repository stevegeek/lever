package agent

import (
	"context"
	"net/http"
)

// Provision mints a one-use enrolment ticket for a worker via the broker's
// /provision endpoint over the caller's mTLS identity. /provision is
// manager-CN-gated by the broker, so client must present the manager identity.
func Provision(ctx context.Context, brokerURL string, client *http.Client, worker string) (string, error) {
	pr, err := postJSON[struct {
		Ticket string `json:"ticket"`
	}](ctx, client, brokerURL+"/provision",
		map[string]string{"worker": worker},
		0, "agent: provision", true)
	if err != nil {
		return "", err
	}
	return pr.Ticket, nil
}

// BootstrapFor composes the worker Bootstrap a freshly-provisioned worker enrols with.
func BootstrapFor(worker, ticket, brokerCA, brokerURL string) Bootstrap {
	return Bootstrap{Ticket: ticket, BrokerCA: brokerCA, BrokerURL: brokerURL, AgentCN: worker}
}
