// Package registry constructs the selected containment backend. It is the one
// place that maps a backend name to a concrete implementation, so adding a
// backend means adding one entry here (and one entry in backend.Candidates) —
// nothing else in the codebase names a concrete backend.
package registry

import (
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/lima"
	"github.com/stevegeek/lever/internal/backend/orbstack"
	"github.com/stevegeek/lever/internal/exec"
)

// Default is the backend used when a caller supplies no name (e.g. the low-level
// `lever provision`, which is flag-driven and has no config).
const Default = "orbstack"

// constructors holds a builder for every candidate backend. A candidate in
// backend.Candidates without an entry here (or vice versa) is a bug caught by
// TestConstructorsMatchCandidates.
var constructors = map[string]func(exec.Runner, string) backend.Backend{
	"orbstack": func(r exec.Runner, machine string) backend.Backend { return orbstack.New(r, machine) },
	"lima":     func(r exec.Runner, machine string) backend.Backend { return lima.New(r, machine) },
}

// Select builds the named backend for a jail machine. An empty name uses
// Default. An unknown name returns an error listing the valid set, so a config
// can never silently run a different backend than it asked for.
func Select(name string, r exec.Runner, machine string) (backend.Backend, error) {
	if name == "" {
		name = Default
	}
	ctor, ok := constructors[name]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (valid: %s)", name, strings.Join(backend.Names(), ", "))
	}
	return ctor(r, machine), nil
}
