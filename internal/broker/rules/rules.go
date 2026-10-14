// Package rules is the broker's default-deny request/delegation policy: per
// agent, which capabilities it may obtain for itself and which it may
// delegate (mint bound to another agent).
package rules

type capKey struct {
	tool string
	op   string
}

type agentPolicy struct {
	obtain   map[capKey]struct{}            // (tool,op) the agent may obtain for itself
	delegate map[capKey]map[string]struct{} // (tool,op) -> recipients it may delegate to
}

// Policy is NOT safe for concurrent mutation: build it fully at boot, then
// query it concurrently. No runtime route changes a grant; a policy change
// means a broker restart.
type Policy struct {
	agents map[string]*agentPolicy
}

// NewPolicy returns an empty, default-deny policy.
func NewPolicy() *Policy {
	return &Policy{agents: map[string]*agentPolicy{}}
}

func (p *Policy) ensure(agent string) *agentPolicy {
	ap, ok := p.agents[agent]
	if !ok {
		ap = &agentPolicy{
			obtain:   map[capKey]struct{}{},
			delegate: map[capKey]map[string]struct{}{},
		}
		p.agents[agent] = ap
	}
	return ap
}

// AllowObtain permits agent to obtain (tool, op) for itself.
func (p *Policy) AllowObtain(agent, tool, op string) {
	p.ensure(agent).obtain[capKey{tool, op}] = struct{}{}
}

// AllowDelegate permits agent to delegate (tool, op) to each recipient in
// `to`. Recipients accumulate across calls.
func (p *Policy) AllowDelegate(agent, tool, op string, to ...string) {
	ap := p.ensure(agent)
	k := capKey{tool, op}
	set, ok := ap.delegate[k]
	if !ok {
		set = map[string]struct{}{}
		ap.delegate[k] = set
	}
	for _, r := range to {
		set[r] = struct{}{}
	}
}

// MayObtainRule reports whether requester may mint a token for (tool, op)
// bound to boundTo, and on allow the matched rule's id for the audit trail.
// requester == boundTo is a self-obtain; otherwise a delegation. Fails
// closed.
func (p *Policy) MayObtainRule(requester, boundTo, tool, op string) (string, bool) {
	ap, ok := p.agents[requester]
	if !ok {
		return "", false
	}
	k := capKey{tool, op}
	if requester == boundTo {
		if _, ok := ap.obtain[k]; !ok {
			return "", false
		}
		return "obtain:" + requester + ":" + tool + "." + op, true
	}
	recips, ok := ap.delegate[k]
	if !ok {
		return "", false
	}
	if _, ok := recips[boundTo]; !ok {
		return "", false
	}
	return "delegate:" + requester + "->" + boundTo + ":" + tool + "." + op, true
}
