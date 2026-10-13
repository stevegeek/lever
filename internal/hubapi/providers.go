package hubapi

import (
	"context"
	"fmt"
	"net/url"
)

// Provider is the part of a project's runtime-broker link lever reads.
type Provider struct {
	BrokerID   string `json:"brokerId"`
	BrokerName string `json:"brokerName"`
}

// ProjectProviders lists the runtime brokers linked to a project, by project
// UUID. A project with none cannot start an agent.
func (c *Client) ProjectProviders(ctx context.Context, projectID string) ([]Provider, error) {
	var body struct {
		Providers *[]Provider `json:"providers"`
	}
	path := "/api/v1/projects/" + url.PathEscape(projectID) + "/providers"
	if err := c.get(ctx, path, &body); err != nil {
		return nil, err
	}
	if body.Providers == nil {
		return nil, &APIError{Msg: fmt.Sprintf("GET %s: the response has no providers list; the hub API changed shape", path)}
	}
	return *body.Providers, nil
}
