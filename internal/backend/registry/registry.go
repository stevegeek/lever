// Package registry constructs the selected containment backend. It is the one
// place that maps a backend name to a concrete implementation, so adding a
// backend means adding one entry here (and flipping its Status to Implemented in
// backend.Candidates) — nothing else in the codebase names a concrete backend.
package registry

import (
	"fmt"
	"strings"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/backend/orbstack"
	"github.com/lever-to/lever/internal/exec"
)

// Default is the backend used when a caller supplies no name (e.g. the low-level
// `lever provision`, which is flag-driven and has no config).
const Default = "orbstack"

// constructors holds a builder for every IMPLEMENTED backend. A candidate marked
// implemented in backend.Candidates without an entry here (or vice versa) is a
// bug caught by TestConstructorsMatchImplementedStatus.
var constructors = map[string]func(exec.Runner, string) backend.Backend{
	"orbstack": func(r exec.Runner, machine string) backend.Backend { return orbstack.New(r, machine) },
}

// Select builds the named backend for a jail machine. An empty name uses Default.
// A planned/experimental backend (declared but unbuilt) and an unknown name both
// return an error naming the selectable set, so a config can never silently run a
// different backend than it asked for.
func Select(name string, r exec.Runner, machine string) (backend.Backend, error) {
	if name == "" {
		name = Default
	}
	c, known := backend.Lookup(name)
	if !known {
		return nil, fmt.Errorf("unknown backend %q (selectable: %s)", name, strings.Join(backend.SelectableNames(), ", "))
	}
	ctor, ok := constructors[name]
	if !ok {
		return nil, fmt.Errorf("backend %q is %s, not yet implemented (selectable: %s)", name, c.Status, strings.Join(backend.SelectableNames(), ", "))
	}
	return ctor(r, machine), nil
}
