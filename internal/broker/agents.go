package broker

import (
	"slices"
	"sync"
)

// agentRegistry holds the workers the broker may drive, each with the spec
// it was built from. Every read goes through it, under its lock, so a later
// change can add and remove agents while the broker serves. Today it holds
// the workers of the config the broker started with.
type agentRegistry struct {
	mu    sync.RWMutex
	specs map[string]WorkerSpec
}

func newAgentRegistry(specs []WorkerSpec) *agentRegistry {
	r := &agentRegistry{specs: make(map[string]WorkerSpec, len(specs))}
	for _, s := range specs {
		r.specs[s.Name] = s
	}
	return r
}

// get returns a copy of name's spec.
func (r *agentRegistry) get(name string) (WorkerSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.specs[name]
	return s, ok
}

func (r *agentRegistry) has(name string) bool {
	_, ok := r.get(name)
	return ok
}

// names returns the registered names, sorted: a snapshot, so a caller that
// ranges over it holds no lock.
func (r *agentRegistry) names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.specs))
	for name := range r.specs {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// put adds or replaces spec.
func (r *agentRegistry) put(spec WorkerSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.specs[spec.Name] = spec
}
