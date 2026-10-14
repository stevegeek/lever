package hubapi

import (
	"context"
	"fmt"
	"net/url"
)

// AgentCapabilities is what the hub says the caller may do in a project: the
// scope actions (create, list) and, per agent slug, the actions on that agent.
// The hub computes both from the same authorization the action routes run, so
// it proves an authority without exercising it.
type AgentCapabilities struct {
	Scope  []string
	Agents map[string][]string
}

// ProjectAgentCapabilities lists the project's agents with the caller's
// capabilities (GET /api/v1/agents?projectId=). A response without the
// capability fields is an error: an empty set would read as "no authority".
//
// The compact view drops each record's applied config (its task text) and
// keeps both capability fields and the cursor. A hub that ignores view sends
// the full records, which decode the same.
func (c *Client) ProjectAgentCapabilities(ctx context.Context, projectID string) (AgentCapabilities, error) {
	ctx = WithReplyCap(ctx, AgentListReplyCap)
	out := AgentCapabilities{Agents: map[string][]string{}}
	cursor := ""
	for page := 0; page < agentPageCap; page++ {
		path := fmt.Sprintf("/api/v1/agents?limit=%d&projectId=%s&view=compact", agentPageLimit, url.QueryEscape(projectID))
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var body struct {
			Agents []struct {
				Slug string `json:"slug"`
				Cap  *struct {
					Actions []string `json:"actions"`
				} `json:"_capabilities"`
			} `json:"agents"`
			Cap *struct {
				Actions []string `json:"actions"`
			} `json:"_capabilities"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.get(ctx, path, &body); err != nil {
			return AgentCapabilities{}, err
		}
		if body.Cap == nil {
			return AgentCapabilities{}, &APIError{Msg: "the agent list carries no _capabilities; lever cannot read the caller's authority"}
		}
		if page == 0 {
			out.Scope = body.Cap.Actions
		}
		for _, a := range body.Agents {
			if a.Cap == nil {
				return AgentCapabilities{}, &APIError{Msg: fmt.Sprintf("agent %q is listed without _capabilities", a.Slug)}
			}
			out.Agents[a.Slug] = a.Cap.Actions
		}
		if body.NextCursor == "" {
			return out, nil
		}
		cursor = body.NextCursor
	}
	return AgentCapabilities{}, &APIError{Msg: fmt.Sprintf("hub still had more agents after %d pages", agentPageCap)}
}
