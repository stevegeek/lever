package hubapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// Agent mirrors the fields of a hub agent record that lever inspects.
type Agent struct {
	// ID is the hub's agent UUID. It is what notification records carry as
	// `agentId`, so it is the only way to attribute an event to an agent.
	ID   string
	Slug string
	Name string
	// Role is the authorization role stored on the record at create. It is
	// empty for a record created by a scion without roles, which current
	// scion resolves to full. Callers must treat empty as unknown authority,
	// never as a default.
	Role string
	// RoleGrandfathered marks a Role scion's migration stamped (full) on a
	// record that stored none. Every consumer of Role must treat it like an
	// empty role.
	RoleGrandfathered bool
	// Phase, Activity and ContainerStatus are the record's state as the hub
	// stores it. They are agent-reported text (an agent may post its own
	// status): label them with scion.PhaseLabel / ActivityLabel before use;
	// never echo them.
	Phase           string
	Activity        string
	ContainerStatus string
	// Volumes are the extra mounts the record was created with; a resume
	// recreates the container from them. Hub-stored text: compare it, never
	// echo it unsanitized.
	Volumes []Volume
}

// Volume is one entry of a record's inline-config volumes (scion's
// api.VolumeMount).
type Volume struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

type wireAgent struct {
	ID              string `json:"id"`
	Slug            string `json:"slug"`
	Name            string `json:"name"`
	Phase           string `json:"phase"`
	Activity        string `json:"activity"`
	ContainerStatus string `json:"containerStatus"`
	AppliedConfig   *struct {
		AgentRole              string `json:"agentRole"`
		AgentRoleGrandfathered bool   `json:"agentRoleGrandfathered"`
		InlineConfig           *struct {
			Volumes []Volume `json:"volumes"`
		} `json:"inlineConfig"`
	} `json:"appliedConfig"`
}

// agentPageLimit keeps an agent list page well under AgentListReplyCap: each
// full record carries its task text, which has no size limit on the hub.
const agentPageLimit = 50

// AgentListReplyCap is the reply cap on one agent list page.
const AgentListReplyCap = 8 << 20

// agentPageCap bounds the cursor loop. Giving up loudly is required: a caller
// handed a truncated list would read a missing agent as nothing to check.
const agentPageCap = 20

// Agents lists a project's agent records, by project name or slug, with the
// role each record stores.
//
// scion list --format json carries no role field, so this is the only way to
// detect a record that stores no role, which scion resolves to full on
// dispatch and on every token refresh.
//
// Requires an agent read scope on the caller's token.
func (c *Client) Agents(ctx context.Context, projectNameOrSlug, endpointHint string) ([]Agent, error) {
	agents, _, err := c.AgentsLargestPage(ctx, projectNameOrSlug, endpointHint)
	return agents, err
}

// AgentsLargestPage is Agents that also reports the size in bytes of the
// largest list page the hub sent, to compare with AgentListReplyCap.
func (c *Client) AgentsLargestPage(ctx context.Context, projectNameOrSlug, endpointHint string) ([]Agent, int, error) {
	id, err := c.ProjectID(ctx, projectNameOrSlug, endpointHint)
	if err != nil {
		return nil, 0, err
	}

	listCtx := WithReplyCap(ctx, AgentListReplyCap)
	var out []Agent
	largest := 0
	cursor := ""
	for page := 0; page < agentPageCap; page++ {
		path := fmt.Sprintf("/api/v1/agents?limit=%d&projectId=%s", agentPageLimit, url.QueryEscape(id))
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}

		// Require the key: a caller reads "no record" as "nothing to check",
		// so a hub that renamed the field would silently disarm the check.
		var raw map[string]json.RawMessage
		n, err := c.getSized(listCtx, path, &raw)
		if err != nil {
			return nil, 0, err
		}
		largest = max(largest, n)
		field, ok := raw["agents"]
		if !ok {
			return nil, 0, &APIError{Msg: fmt.Sprintf(
				"agent list response for project %q has no \"agents\" field; the hub API changed shape and lever cannot confirm what authority its agents hold",
				projectNameOrSlug)}
		}
		var records []wireAgent
		if err := json.Unmarshal(field, &records); err != nil {
			return nil, 0, &APIError{Msg: fmt.Sprintf("decoding agents: %v", err)}
		}
		for _, a := range records {
			rec := Agent{ID: a.ID, Slug: a.Slug, Name: a.Name,
				Phase: a.Phase, Activity: a.Activity, ContainerStatus: a.ContainerStatus}
			if a.AppliedConfig != nil {
				rec.Role = a.AppliedConfig.AgentRole
				rec.RoleGrandfathered = a.AppliedConfig.AgentRoleGrandfathered
				if a.AppliedConfig.InlineConfig != nil {
					rec.Volumes = a.AppliedConfig.InlineConfig.Volumes
				}
			}
			out = append(out, rec)
		}

		next := ""
		if rawCursor, ok := raw["nextCursor"]; ok {
			if err := json.Unmarshal(rawCursor, &next); err != nil {
				return nil, 0, &APIError{Msg: fmt.Sprintf("decoding nextCursor: %v", err)}
			}
		}
		if next == "" {
			return out, largest, nil
		}
		cursor = next
	}
	return nil, 0, &APIError{Msg: fmt.Sprintf(
		"hub still had more agents for project %q after %d pages; refusing to act on a truncated list",
		projectNameOrSlug, agentPageCap)}
}

// VerifyAgentRole refuses to KEEP an agent record that stores no role while the
// installed scion resolves that to full hub authority.
//
// Every path that resumes or retains an existing agent must call it.
// rolesSupported asks the installed binary, not the pin, whether roles exist.
//
// Fails closed on every question it cannot answer: guessing hands out
// authority.
func VerifyAgentRole(ctx context.Context, rolesSupported func(context.Context) (bool, error), c *Client, projectName, agentName string) error {
	roles, err := rolesSupported(ctx)
	if err != nil {
		return fmt.Errorf("cannot tell whether the installed scion understands agent roles, "+
			"so cannot tell what authority %q would resume with: %w", agentName, err)
	}
	if !roles {
		// A scion without roles has no role-derived scopes to widen.
		return nil
	}
	agents, err := c.Agents(ctx, projectName, "the instance hub")
	if err != nil {
		return fmt.Errorf("reading the hub's agent records to check %q's stored role: %w", agentName, err)
	}
	// An absent record is not this guard's business: the caller creates one, and
	// the create path stamps a role.
	rec, found := FindAgent(agents, agentName)
	if !found {
		return nil
	}
	if rec.RoleGrandfathered {
		return fmt.Errorf("agent %q stores role %q only because scion's role migration grandfathered it there: the record was created by a scion older than agent roles (scion#1089), "+
			"and the migration stamps FULL hub authority (agent create, agent lifecycle, project-secret-read) on every such record, "+
			"which is the same promotion an unset role gets.\n"+
			"  A stored role is immutable and `scion resume` takes no --role flag, so lever cannot repair this for you.\n"+
			"  Either delete the agent so lever recreates it (it stamps --role baseline, but the conversation is LOST), "+
			"or pin a scion older than the migration until you are ready to lose the session", agentName, rec.Role)
	}
	if rec.Role != "" {
		return nil
	}
	return fmt.Errorf("agent %q has no role stored on its hub record, and this scion resolves that to FULL hub authority "+
		"(agent create, agent lifecycle, project-secret-read) on dispatch and on every token refresh — scion#1090, #1101, #1102.\n"+
		"  The record was created by a scion older than agent roles (scion#1089). A role is written only when an agent is CREATED, is immutable after, "+
		"and `scion resume` takes no --role flag, so lever cannot repair this for you.\n"+
		"  Either delete the agent so lever recreates it (it stamps --role baseline, but the conversation is LOST), "+
		"or pin a scion older than scion#1089 until you are ready to lose the session", agentName)
}

// AgentID resolves an agent slug (or name) to the hub's agent UUID. An agent
// the hub does not list is an error, never an empty id: a caller that treated
// "" as "no filter" would show one agent another's events.
func (c *Client) AgentID(ctx context.Context, projectNameOrSlug, endpointHint, agentSlug string) (string, error) {
	agents, err := c.Agents(ctx, projectNameOrSlug, endpointHint)
	if err != nil {
		return "", err
	}
	rec, found := FindAgent(agents, agentSlug)
	if !found {
		return "", &APIError{Msg: fmt.Sprintf("hub lists no agent %q in project %q", agentSlug, projectNameOrSlug)}
	}
	if rec.ID == "" {
		return "", &APIError{Msg: fmt.Sprintf("hub record for agent %q carries no id", agentSlug)}
	}
	return rec.ID, nil
}

// FindAgent returns the record for slug (or name), and whether one exists.
func FindAgent(agents []Agent, slugOrName string) (Agent, bool) {
	for _, a := range agents {
		if a.Slug == slugOrName || a.Name == slugOrName {
			return a, true
		}
	}
	return Agent{}, false
}
