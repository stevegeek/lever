package hubapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// RoleCeiling is the pair of project settings that bound agent roles hub-side.
// Max caps the role any create may request (scion's max_agent_role); Default
// is the role a create gets when it names none. Empty means unset, which scion
// reads as full.
type RoleCeiling struct {
	Max     string
	Default string
}

// settings is read-only: writes go through a raw map (see
// EnsureAgentRoleCeiling) so an upstream field lever does not know cannot be
// dropped.
type settings struct {
	MaxAgentRole     string `json:"maxAgentRole"`
	DefaultAgentRole string `json:"defaultAgentRole"`
}

func settingsPath(projectID string) string {
	return "/api/v1/projects/" + url.PathEscape(projectID) + "/settings"
}

// AgentRoleCeiling reads the project's role ceiling by project name or slug.
func (c *Client) AgentRoleCeiling(ctx context.Context, projectNameOrSlug, endpointHint string) (RoleCeiling, error) {
	id, err := c.ProjectID(ctx, projectNameOrSlug, endpointHint)
	if err != nil {
		return RoleCeiling{}, err
	}
	var s settings
	if err := c.get(ctx, settingsPath(id), &s); err != nil {
		return RoleCeiling{}, err
	}
	return RoleCeiling{Max: s.MaxAgentRole, Default: s.DefaultAgentRole}, nil
}

// EnsureAgentRoleCeiling sets both the maximum and the default agent role of
// a project to role, by project name or slug, and VERIFIES the hub kept them.
// It reports whether it wrote anything.
//
// With the ceiling in place the hub itself refuses a create above it, whoever
// asks (for example a manager with a stolen controller PAT); otherwise
// lever's --role stamp on scion start is the only bound.
//
// scion's settings route is a whole-object PUT that deletes any field absent
// from the body, so the write is a read-modify-write over the raw object and
// needs a BodyDoer. Requires project:update on the caller's token.
func (c *Client) EnsureAgentRoleCeiling(ctx context.Context, projectNameOrSlug, endpointHint, role string) (bool, error) {
	id, err := c.ProjectID(ctx, projectNameOrSlug, endpointHint)
	if err != nil {
		return false, err
	}
	path := settingsPath(id)
	var raw map[string]json.RawMessage
	if err := c.get(ctx, path, &raw); err != nil {
		return false, err
	}
	if raw == nil {
		raw = map[string]json.RawMessage{}
	}
	if fieldIs(raw, "maxAgentRole", role) && fieldIs(raw, "defaultAgentRole", role) {
		return false, nil
	}
	if _, ok := c.T.(BodyDoer); !ok {
		return false, fmt.Errorf("hubapi: cannot write project settings: transport has no body path")
	}
	want, _ := json.Marshal(role)
	raw["maxAgentRole"] = want
	raw["defaultAgentRole"] = want
	body, err := json.Marshal(raw)
	if err != nil {
		return false, fmt.Errorf("hubapi: encoding project settings: %w", err)
	}
	status, resp, err := c.doBody(ctx, http.MethodPut, path, body)
	if err != nil {
		return false, err
	}
	if status < 200 || status > 299 {
		return false, &APIError{Status: status,
			Msg: fmt.Sprintf("PUT %s: HTTP %d: %s", path, status, snippet(resp))}
	}
	// A 200 with nothing stored must not read as done.
	var got settings
	if err := c.get(ctx, path, &got); err != nil {
		return false, fmt.Errorf("verifying the agent-role ceiling on project %q: %w", projectNameOrSlug, err)
	}
	if got.MaxAgentRole != role || got.DefaultAgentRole != role {
		return false, &APIError{Status: status,
			Msg: fmt.Sprintf("project %q settings after PUT returned HTTP %d: maxAgentRole=%q defaultAgentRole=%q, want both %q (the hub did not keep the ceiling)",
				projectNameOrSlug, status, got.MaxAgentRole, got.DefaultAgentRole, role)}
	}
	return true, nil
}

func fieldIs(raw map[string]json.RawMessage, key, want string) bool {
	v, ok := raw[key]
	if !ok {
		return false
	}
	var s string
	return json.Unmarshal(v, &s) == nil && s == want
}
