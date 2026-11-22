// Package rules is the broker's default-deny request/delegation policy: per
// agent, which capabilities it may obtain for itself and which it may
// delegate (mint bound to another agent).
package rules

import (
	"strings"
	"sync"
)

type capKey struct {
	tool string
	op   string
}

type agentPolicy struct {
	obtain   map[capKey]struct{}            // (tool,op) the agent may obtain for itself
	delegate map[capKey]map[string]struct{} // (tool,op) -> recipients it may delegate to
}

// TemplateRefPrefix starts a delegate recipient that names every active
// instance of a template ("template:<name>"); config.TemplateRefPrefix.
const TemplateRefPrefix = "template:"

// Policy is safe for concurrent use. The config's agents are added at boot;
// a spawned agent is added when it is spawned and removed when it is
// destroyed, so its grants exist exactly while it does.
type Policy struct {
	mu     sync.RWMutex
	agents map[string]*agentPolicy
	// templateOf resolves a recipient to the template of its active
	// instance (SetTemplateResolver). nil: no recipient is an instance.
	templateOf func(agent string) (string, bool)
}

// NewPolicy returns an empty, default-deny policy.
func NewPolicy() *Policy {
	return &Policy{agents: map[string]*agentPolicy{}}
}

// Grant is one (tool, op) an agent may obtain.
type Grant struct{ Tool, Op string }

// Delegation is one (tool, op) an agent may delegate to each of To. A
// recipient may be TemplateRefPrefix+name.
type Delegation struct {
	Tool, Op string
	To       []string
}

// SetTemplateResolver sets how a "template:<name>" recipient is matched:
// templateOf(boundTo) must report boundTo's template, and only for an
// active instance. Fails closed while unset.
func (p *Policy) SetTemplateResolver(templateOf func(agent string) (string, bool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.templateOf = templateOf
}

func (p *Policy) ensureLocked(agent string) *agentPolicy {
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
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked(agent).obtain[capKey{tool, op}] = struct{}{}
}

// AllowDelegate permits agent to delegate (tool, op) to each recipient in
// `to`. Recipients accumulate across calls.
func (p *Policy) AllowDelegate(agent, tool, op string, to ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.allowDelegateLocked(agent, tool, op, to)
}

func (p *Policy) allowDelegateLocked(agent, tool, op string, to []string) {
	ap := p.ensureLocked(agent)
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

// AddAgent replaces agent's grants with exactly obtain and delegate, in one
// step: no reader sees a partial set.
func (p *Policy) AddAgent(agent string, obtain []Grant, delegate []Delegation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.agents, agent)
	ap := p.ensureLocked(agent)
	for _, g := range obtain {
		ap.obtain[capKey{g.Tool, g.Op}] = struct{}{}
	}
	for _, d := range delegate {
		p.allowDelegateLocked(agent, d.Tool, d.Op, d.To)
	}
}

// RemoveAgent drops every grant of agent: it may obtain and delegate
// nothing. A grant naming it as a recipient stays but matches only while
// it is an active instance (template recipients) or by exact name, which a
// destroyed name never gets back.
func (p *Policy) RemoveAgent(agent string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.agents, agent)
}

// MayObtainRule reports whether requester may mint a token for (tool, op)
// bound to boundTo, and on allow the matched rule's id for the audit trail.
// requester == boundTo is a self-obtain; otherwise a delegation. Fails
// closed.
func (p *Policy) MayObtainRule(requester, boundTo, tool, op string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
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
	if _, ok := recips[boundTo]; ok && !strings.HasPrefix(boundTo, TemplateRefPrefix) {
		return "delegate:" + requester + "->" + boundTo + ":" + tool + "." + op, true
	}
	if p.templateOf == nil {
		return "", false
	}
	tmpl, ok := p.templateOf(boundTo)
	if !ok {
		return "", false
	}
	if _, ok := recips[TemplateRefPrefix+tmpl]; !ok {
		return "", false
	}
	return "delegate:" + requester + "->" + boundTo + " (" + TemplateRefPrefix + tmpl + "):" + tool + "." + op, true
}
