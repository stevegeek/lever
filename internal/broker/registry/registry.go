// Package registry holds the broker's view of registered MCP tools: their
// backends, operations, the caveat→param mapping that turns a request's real
// arguments into the param set the token layer verifies, and optional
// allowed-value rules.
//
// Unlike rules.Policy it is mutated while the broker serves (first-party
// tools register through /register), so it is concurrency-safe.
package registry

import (
	"fmt"
	"slices"
	"sync"

	"github.com/stevegeek/lever/internal/mcp"
	"github.com/stevegeek/lever/internal/wire"
)

// Reserved names of the LLM pseudo-tool and of a coarse tool's single
// operation, declared in wire so config can use them without this package.
const (
	ReservedLLMTool = wire.ReservedLLMTool
	ReservedLLMOp   = wire.ReservedLLMOp
	WildcardOp      = wire.WildcardOp
)

// Operation describes one operation a tool exposes.
type Operation struct {
	Name string
	// CaveatParam maps a constraint key to the argument it constrains (e.g.
	// "table" ⇒ "schema.table"). Absent keys are identity-mapped.
	CaveatParam map[string]string
	// Params optionally declares the argument names. When non-empty,
	// ValidateConstraints rejects a constraint key outside Params ∪
	// CaveatParam at mint time, instead of minting a token that can never
	// match. Empty = permissive.
	Params []string
}

// Tool is a registered MCP tool.
type Tool struct {
	Name       string
	Backend    string
	Operations map[string]Operation
	// AllowedValues optionally restricts a constraint key to a permitted set.
	// An absent key is unrestricted here; the tool still backstops.
	AllowedValues map[string][]string
	// FirstParty marks a tool that verifies tokens itself (captool); the
	// gateway forwards the token and caller to it instead of stripping them.
	FirstParty bool
	// External marks a host server the broker fronts but does not spawn. It
	// is registered from config, and /register rejects it.
	External bool
	// Coarse makes the gateway require {tool, WildcardOp} for every
	// tools/call.
	Coarse bool
}

// CloneAllowedValues deep-copies m so a registry entry never aliases the
// caller's map.
func CloneAllowedValues(m map[string][]string) map[string][]string {
	if m == nil {
		return nil
	}
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = slices.Clone(v)
	}
	return out
}

// Registry is the concurrency-safe set of registered tools.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register records (or replaces, by name) a tool. It takes ownership of t's
// nested maps; callers must not mutate them afterwards.
func (r *Registry) Register(t Tool) error {
	if t.Name == "" {
		return fmt.Errorf("registry: tool has empty name")
	}
	if t.Backend == "" {
		return fmt.Errorf("registry: tool %q has empty backend", t.Name)
	}
	if len(t.Operations) == 0 {
		return fmt.Errorf("registry: tool %q has no operations", t.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name] = t
	return nil
}

// Lookup returns a registered tool by name.
func (r *Registry) Lookup(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// HasOperation reports whether tool is registered and exposes op.
func (r *Registry) HasOperation(tool, op string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[tool]
	if !ok {
		return false
	}
	_, ok = t.Operations[op]
	return ok
}

// Names returns the registered tool names in unspecified order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	return names
}

// MapParams builds the constraint-keyed param set the token layer verifies,
// from the request's MCP arguments. The mapping lives in mcp because the
// broker and captool must compute byte-identical param sets.
func (r *Registry) MapParams(tool, op string, args map[string]string) (map[string]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[tool]
	if !ok {
		return nil, fmt.Errorf("registry: unknown tool %q", tool)
	}
	o, ok := t.Operations[op]
	if !ok {
		return nil, fmt.Errorf("registry: tool %q has no operation %q", tool, op)
	}
	return mcp.MapConstraintParams(o.CaveatParam, args), nil
}

// ValidateConstraints is the mint-time defense-in-depth check: a constraint
// value AllowedValues forbids, or a key outside a declared Params set, is an
// error.
func (r *Registry) ValidateConstraints(tool, op string, constraints map[string]string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[tool]
	if !ok {
		return fmt.Errorf("registry: unknown tool %q", tool)
	}
	o, ok := t.Operations[op]
	if !ok {
		return fmt.Errorf("registry: tool %q has no operation %q", tool, op)
	}
	if len(o.Params) > 0 {
		declared := make(map[string]bool, len(o.Params)+len(o.CaveatParam))
		for _, p := range o.Params {
			declared[p] = true
		}
		for ck := range o.CaveatParam {
			declared[ck] = true
		}
		for k := range constraints {
			if !declared[k] {
				return fmt.Errorf("registry: %s.%s has no parameter %q (declared: %v) — a constraint on it could never match a call", tool, op, k, o.Params)
			}
		}
	}
	for k, v := range constraints {
		allowed, restricted := t.AllowedValues[k]
		if !restricted {
			continue
		}
		if !slices.Contains(allowed, v) {
			return fmt.Errorf("registry: tool %q forbids %s=%q", tool, k, v)
		}
	}
	return nil
}
