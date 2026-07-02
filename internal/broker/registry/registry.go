// Package registry holds the broker's view of registered MCP tools: their
// backends, operations, the caveat→param mapping that lets the gateway turn a
// request's real arguments into the constraint-keyed param set the token layer
// verifies, and optional allowed-value rules. The registry is concurrency-safe; tools register at boot.
package registry

import (
	"fmt"
	"sync"
)

// Operation describes one operation a tool exposes.
type Operation struct {
	Name string
	// CaveatParam maps a token constraint key to the request parameter name it
	// constrains, for non-identity mappings (e.g. constraint "table" ⇒ arg
	// "schema.table"). Constraint keys absent here are identity-mapped
	// (constraint key == arg name).
	CaveatParam map[string]string
}

// WildcardOp is the operation name of a coarse tool's single capability. It is
// a literal op value — minted, granted, and verified by exact match — so the
// token layer needs no wildcard logic; the gateway simply REQUIRES this op for
// a coarse tool (and never for a fine one, so "*" cannot widen a fine tool).
const WildcardOp = "*"

// Tool is a registered MCP tool.
type Tool struct {
	Name       string
	Backend    string
	Operations map[string]Operation
	// AllowedValues optionally restricts a constraint key to a permitted set
	// (e.g. table ∈ {A,B}). An absent key is unrestricted at this layer; the
	// tool still backstops in its own logic.
	AllowedValues map[string][]string
	// FirstParty marks a capability-aware tool that runs the captool SDK and
	// verifies tokens itself; the gateway forwards the token + caller to it
	// rather than stripping (see the gateway). Third-party tools leave this false.
	FirstParty bool
	// External marks an already-running host server the broker fronts but does
	// not spawn or expect to self-register: it is registered from config at
	// boot, and /register rejects it.
	External bool
	// Coarse marks an external tool gated by the single wildcard capability
	// ({tool, WildcardOp}): the gateway requires that capability for every
	// tools/call instead of the per-MCP-tool operation.
	Coarse bool
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

// Register records (or replaces, by name) a tool. Name, Backend, and at least
// one operation are required. Register takes ownership of t's nested maps
// (Operations, AllowedValues, and each Operation's CaveatParam); callers must
// not mutate them after registering.
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

// Names returns the names of all registered tools in unspecified order.
// It is safe for concurrent use.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	return names
}

// MapParams builds the constraint-keyed parameter set the token layer verifies
// against, from the request's actual MCP arguments. Arguments are identity-
// mapped (constraint key == arg name); the operation's CaveatParam entries add
// renamed bindings (constraint key -> the value of a differently-named arg). A
// renamed arg that is absent produces no binding, so a token constraint on that
// key then fails closed at verification. Errors if tool/op is unregistered.
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
	out := make(map[string]string, len(args)+len(o.CaveatParam))
	for k, v := range args { // identity mapping (constraint key == arg name)
		out[k] = v
	}
	for ck, argName := range o.CaveatParam { // renamed bindings
		if v, ok := args[argName]; ok {
			out[ck] = v
		}
	}
	return out, nil
}

// ValidateConstraints returns an error if any (key,value) constraint requests a
// value the tool's AllowedValues forbids. Keys without an AllowedValues entry
// are unrestricted at this layer (the tool backstops). Used at mint time as
// defense-in-depth. Errors if the tool is unregistered.
func (r *Registry) ValidateConstraints(tool string, constraints map[string]string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[tool]
	if !ok {
		return fmt.Errorf("registry: unknown tool %q", tool)
	}
	for k, v := range constraints {
		allowed, restricted := t.AllowedValues[k]
		if !restricted {
			continue
		}
		found := false
		for _, a := range allowed {
			if a == v {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("registry: tool %q forbids %s=%q", tool, k, v)
		}
	}
	return nil
}
