// Package registry constructs the selected containment backend and its jail
// transport. It is the ONE place that names a concrete backend: the backends
// table below maps a name to its constructor and jail-prefix function, and
// the lockstep test keeps it equal to backend.Candidates (the guarantee
// matrix, which lives in the leaf package and cannot import implementations).
package registry

import (
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/lima"
	"github.com/stevegeek/lever/internal/backend/orbstack"
	"github.com/stevegeek/lever/internal/exec"
	"github.com/stevegeek/lever/internal/jail"
)

// Default is the backend used when a caller supplies no name (e.g. the low-level
// `lever provision`, which is flag-driven and has no config).
const Default = "orbstack"

// entry is what the registry knows about one backend.
type entry struct {
	// New builds the backend for a jail machine.
	New func(r exec.Runner, machine string) backend.Backend
	// JailPrefix is the argv prefix that runs a command INSIDE the jail for an
	// already-resolved identity — no EnsureUp state, no I/O.
	JailPrefix func(machine, user string) []string
}

// backends is the single table; adding a backend means one entry here and one
// in backend.Candidates.
var backends = map[string]entry{
	"orbstack": {
		New:        func(r exec.Runner, machine string) backend.Backend { return orbstack.New(r, machine) },
		JailPrefix: orbstack.JailPrefix,
	},
	"lima": {
		New:        func(r exec.Runner, machine string) backend.Backend { return lima.New(r, machine) },
		JailPrefix: func(machine, _ string) []string { return lima.JailPrefix(machine) },
	},
}

// lookup resolves a name (empty → Default) to its entry, or an error listing
// the valid set so a config can never silently run a different backend than it
// asked for.
func lookup(name string) (entry, error) {
	if name == "" {
		name = Default
	}
	e, ok := backends[name]
	if !ok {
		return entry{}, fmt.Errorf("unknown backend %q (valid: %s)", name, strings.Join(backend.Names(), ", "))
	}
	return e, nil
}

// Select builds the named backend for a jail machine. An empty name uses
// Default.
func Select(name string, r exec.Runner, machine string) (backend.Backend, error) {
	e, err := lookup(name)
	if err != nil {
		return nil, err
	}
	return e.New(r, machine), nil
}

// JailArgv returns the argv prefix that runs a command INSIDE the jail for the
// named backend, from an already-resolved identity. The remote proxy's
// jail-dial transport needs the bare prefix rather than a JailRunner: it execs
// `<prefix> nc host port` and adapts the child's pipes to a net.Conn, which
// exec.Runner's run-to-completion, capture-the-output contract cannot express.
func JailArgv(name, machine, user string) ([]string, error) {
	e, err := lookup(name)
	if err != nil {
		return nil, err
	}
	return e.JailPrefix(machine, user), nil
}

// JailRunner rebuilds the command transport into a jail from its already-
// resolved identity (machine, run user, uid) WITHOUT needing EnsureUp state.
// The broker uses this for host-side worker dispatch: `lever apply` resolved the
// identity and passed it via env; the broker process reconstructs the transport
// here. The host-network escape hatch is read here, at construction, so the
// transport itself stays pure.
func JailRunner(name string, host exec.Runner, machine, user, uid string) (exec.Runner, error) {
	argv, err := JailArgv(name, machine, user)
	if err != nil {
		return nil, err
	}
	return jail.New(jail.Config{
		Host:             host,
		Prefix:           argv,
		UID:              uid,
		ForceHostNetwork: jail.ForceHostNetworkFromEnv(),
	}), nil
}
