// Package rules is the broker's request/delegation policy: per agent, which
// capabilities it may obtain for itself and which it may delegate (mint bound
// to another agent). It is the delegation/obtain policy. The
// policy is default-deny; build it with AllowObtain/AllowDelegate (typically
// from lever.yaml) and query it with MayObtain.
package rules

type capKey struct {
	tool string
	op   string
}

type agentPolicy struct {
	obtain   map[capKey]struct{}            // (tool,op) the agent may obtain for itself
	delegate map[capKey]map[string]struct{} // (tool,op) -> recipients it may delegate to
}

// Policy holds the per-agent request/delegation policy. It is NOT safe for
// concurrent mutation: build it fully at boot with AllowObtain/AllowDelegate,
// then query MayObtain concurrently.
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

// AllowObtain permits agent to obtain (mint for itself) the (tool, op) capability.
func (p *Policy) AllowObtain(agent, tool, op string) {
	p.ensure(agent).obtain[capKey{tool, op}] = struct{}{}
}

// AllowDelegate permits agent to delegate the (tool, op) capability to each
// recipient in `to` (mint a token bound to that recipient). Recipients
// accumulate across calls.
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

// MayObtain reports whether requester may mint a token for (tool, op) bound to
// boundTo. requester == boundTo is a self-obtain (checked against the obtain
// set); otherwise it is a delegation (checked against the delegate set and its
// recipient list). Fails closed.
func (p *Policy) MayObtain(requester, boundTo, tool, op string) bool {
	_, ok := p.MayObtainRule(requester, boundTo, tool, op)
	return ok
}

// MayObtainRule is MayObtain plus, on allow, a stable identifier of the
// matched policy rule ("obtain:<agent>:<tool>.<op>" or
// "delegate:<agent>-><recipient>:<tool>.<op>") for the audit trail. Denied
// requests return ("", false).
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
