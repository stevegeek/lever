package hubapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// Agent mirrors the fields of a hub agent record that lever inspects.
type Agent struct {
	Slug string
	Name string
	// Role is the authorization role stored on the record when it was created
	// (scion#1089). It is EMPTY for a record created by a scion that predates
	// roles — which scion#1102 resolves to `full`, not `baseline`. Callers must
	// treat empty as "unknown authority", never as a default.
	Role string
}

// wireAgent decodes the subset of the hub's agent record lever reads. The hub
// embeds its whole store.Agent in the list response, so this deliberately names
// only the three fields rather than mirroring a large upstream struct.
type wireAgent struct {
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	AppliedConfig *struct {
		AgentRole string `json:"agentRole"`
	} `json:"appliedConfig"`
}

// agentPageLimit is what lever asks for per page. It matches the hub's own
// default; a lever project holds a manager and a handful of workers, so one
// page always suffices in practice and the cursor loop is the safety net.
const agentPageLimit = 500

// agentPageCap bounds the cursor loop. A hub that keeps returning a cursor
// would otherwise spin forever; giving up loudly is correct, because a caller
// that received a truncated list would read a missing agent as "nothing to
// check".
const agentPageCap = 20

// Agents lists a project's agent records, by project name or slug, with the
// role each record stores.
//
// The stored role is not available any other way: `scion list --format json`
// renders api.AgentInfo, which carries no role field, while this endpoint
// embeds the hub's whole agent record. Reading it is how lever detects a record
// created before scion#1089 — such a record stores no role at all, and a scion
// at or after scion#1102 resolves an unset stored role to `full` on dispatch
// and on every token refresh (scion#1101).
//
// Requires an agent read scope on the caller's token.
func (c *Client) Agents(ctx context.Context, projectNameOrSlug, endpointHint string) ([]Agent, error) {
	id, err := c.ProjectID(ctx, projectNameOrSlug, endpointHint)
	if err != nil {
		return nil, err
	}

	var out []Agent
	cursor := ""
	for page := 0; page < agentPageCap; page++ {
		path := fmt.Sprintf("/api/v1/agents?limit=%d&projectId=%s", agentPageLimit, url.QueryEscape(id))
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}

		// Decode loosely and REQUIRE the key, exactly as SharedDirs does: a
		// caller reads "no record for this agent" as "nothing to check", so a
		// hub that renamed the field would silently disarm the check.
		var raw map[string]json.RawMessage
		if err := c.get(ctx, path, &raw); err != nil {
			return nil, err
		}
		field, ok := raw["agents"]
		if !ok {
			return nil, &APIError{Msg: fmt.Sprintf(
				"agent list response for project %q has no \"agents\" field; the hub API changed shape and lever cannot confirm what authority its agents hold",
				projectNameOrSlug)}
		}
		var records []wireAgent
		if err := json.Unmarshal(field, &records); err != nil {
			return nil, &APIError{Msg: fmt.Sprintf("decoding agents: %v", err)}
		}
		for _, a := range records {
			rec := Agent{Slug: a.Slug, Name: a.Name}
			if a.AppliedConfig != nil {
				rec.Role = a.AppliedConfig.AgentRole
			}
			out = append(out, rec)
		}

		next := ""
		if rawCursor, ok := raw["nextCursor"]; ok {
			if err := json.Unmarshal(rawCursor, &next); err != nil {
				return nil, &APIError{Msg: fmt.Sprintf("decoding nextCursor: %v", err)}
			}
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
	return nil, &APIError{Msg: fmt.Sprintf(
		"hub still had more agents for project %q after %d pages; refusing to act on a truncated list",
		projectNameOrSlug, agentPageCap)}
}

// VerifyAgentRole refuses to KEEP an agent record that stores no role while the
// installed scion resolves that to full hub authority.
//
// It is the one implementation behind every lever path that resumes or retains
// an existing agent: apply's start-manager, `lever up`'s own suspended/running
// fast paths, and the broker's worker resume. rolesSupported asks the installed
// binary (not the pin) whether roles exist at all.
//
// Fails CLOSED on every question it cannot answer. Not knowing whether scion
// has roles, and not being able to read the record, are both states in which
// guessing hands out authority.
func VerifyAgentRole(ctx context.Context, rolesSupported func(context.Context) (bool, error), c *Client, projectName, agentName string) error {
	roles, err := rolesSupported(ctx)
	if err != nil {
		return fmt.Errorf("cannot tell whether the installed scion understands agent roles, "+
			"so cannot tell what authority %q would resume with: %w", agentName, err)
	}
	if !roles {
		// Pre-scion#1089: no record stores a role and none needs to, because
		// there are no role-derived scopes to widen.
		return nil
	}
	agents, err := c.Agents(ctx, projectName, "the instance hub")
	if err != nil {
		return fmt.Errorf("reading the hub's agent records to check %q's stored role: %w", agentName, err)
	}
	// An absent record is not this guard's business: the caller creates one, and
	// the create path stamps a role.
	if rec, found := FindAgent(agents, agentName); !found || rec.Role != "" {
		return nil
	}
	return fmt.Errorf("agent %q has no role stored on its hub record, and this scion resolves that to FULL hub authority "+
		"(agent create, agent lifecycle, project-secret-read) on dispatch and on every token refresh — scion#1090, #1101, #1102.\n"+
		"  The record was created by a scion older than agent roles (scion#1089). A role is written only when an agent is CREATED, is immutable after, "+
		"and `scion resume` takes no --role flag, so lever cannot repair this for you.\n"+
		"  Either delete the agent so lever recreates it (it stamps --role baseline, but the conversation is LOST), "+
		"or pin a scion older than scion#1089 until you are ready to lose the session", agentName)
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
