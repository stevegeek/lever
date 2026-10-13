package hubapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Project mirrors the fields of scion's store.Project that the identity
// steps read.
type Project struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	Slug                   string `json:"slug"`
	OwnerID                string `json:"ownerId,omitempty"`
	CreatedBy              string `json:"createdBy,omitempty"`
	DefaultRuntimeBrokerID string `json:"defaultRuntimeBrokerId,omitempty"`
}

// RegisterProject finds or creates the hub project with the given id
// (POST /api/v1/projects/register); created reports which. The hub also
// matches an existing project by slug, so a different project answering for
// id is an error rather than a result.
func (c *Client) RegisterProject(ctx context.Context, id, name, path string) (Project, bool, error) {
	if id == "" || name == "" {
		return Project{}, false, errors.New("hubapi: registering a project needs an id and a name")
	}
	var body struct {
		Project *Project `json:"project"`
		Created *bool    `json:"created"`
	}
	err := c.send(ctx, http.MethodPost, "/api/v1/projects/register", struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Path string `json:"path,omitempty"`
	}{id, name, path}, &body)
	if err != nil {
		return Project{}, false, err
	}
	if body.Project == nil || body.Created == nil {
		return Project{}, false, &APIError{Msg: "project register response has no \"project\" or \"created\" field; the hub API changed shape"}
	}
	if body.Project.ID != id {
		return Project{}, false, &APIError{Msg: fmt.Sprintf("registering project %s (%q): the hub answered with project %q", id, name, body.Project.ID)}
	}
	return *body.Project, *body.Created, nil
}

// GetProject reads one project by ID (GET /api/v1/projects/{id}); found is
// false on 404.
func (c *Client) GetProject(ctx context.Context, id string) (Project, bool, error) {
	var p Project
	err := c.get(ctx, "/api/v1/projects/"+url.PathEscape(id), &p)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return Project{}, false, nil
	}
	if err != nil {
		return Project{}, false, err
	}
	if p.ID != id {
		return Project{}, false, &APIError{Msg: fmt.Sprintf("GET project %s returned project %q", id, p.ID)}
	}
	return p, true, nil
}

// RuntimeBroker is a runtime broker as the hub lists it, hub-wide or as one
// project's provider.
type RuntimeBroker struct {
	ID     string
	Name   string
	Status string
}

// RuntimeBrokers lists the hub's runtime brokers (GET /api/v1/runtime-brokers).
// A project-scoped PAT sees none of them; use an assertion, or
// ProjectProviders.
func (c *Client) RuntimeBrokers(ctx context.Context) ([]RuntimeBroker, error) {
	var raw map[string]json.RawMessage
	if err := c.get(ctx, "/api/v1/runtime-brokers", &raw); err != nil {
		return nil, err
	}
	field, ok := raw["brokers"]
	if !ok {
		return nil, &APIError{Msg: "runtime broker list has no \"brokers\" field; the hub API changed shape"}
	}
	var wire []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(field, &wire); err != nil {
		return nil, &APIError{Msg: fmt.Sprintf("decoding runtime broker list: %v", err)}
	}
	out := make([]RuntimeBroker, 0, len(wire))
	for _, b := range wire {
		out = append(out, RuntimeBroker{ID: b.ID, Name: b.Name, Status: b.Status})
	}
	return out, nil
}

// ProjectProviders lists the brokers that provide for one project
// (GET /api/v1/projects/{id}/providers).
func (c *Client) ProjectProviders(ctx context.Context, projectID string) ([]RuntimeBroker, error) {
	var raw map[string]json.RawMessage
	if err := c.get(ctx, "/api/v1/projects/"+url.PathEscape(projectID)+"/providers", &raw); err != nil {
		return nil, err
	}
	field, ok := raw["providers"]
	if !ok {
		return nil, &APIError{Msg: "project provider list has no \"providers\" field; the hub API changed shape"}
	}
	var wire []struct {
		BrokerID   string `json:"brokerId"`
		BrokerName string `json:"brokerName"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(field, &wire); err != nil {
		return nil, &APIError{Msg: fmt.Sprintf("decoding project provider list: %v", err)}
	}
	out := make([]RuntimeBroker, 0, len(wire))
	for _, p := range wire {
		out = append(out, RuntimeBroker{ID: p.BrokerID, Name: p.BrokerName, Status: p.Status})
	}
	return out, nil
}
