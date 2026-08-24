// Package registry constructs the selected containment backend and its jail
// transport. It is the ONE place that names a concrete backend: the backends
// table below maps a name to its declared guarantees, constructor and
// jail-prefix function, and Candidates/ProfileFor/Names are views of that
// table. Roadmap and rejected backends are documentation, not code — see
// docs-site/_reference/backends.md, which also states the contract's
// guarantee 0: a hypervisor boundary between the agent workload and the host
// kernel is mandatory; no backend without it may be added here.
package registry

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/backend/lima"
	"github.com/stevegeek/lever/internal/backend/orbstack"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
)

// Default is the backend used when a caller supplies no name (e.g. the low-level
// `lever provision`, which is flag-driven and has no config).
const Default = "orbstack"

// entry is what the registry knows about one backend.
type entry struct {
	// Profile is the guarantees the backend declares; its Name is the key.
	Profile backend.Profile
	// New builds the backend for a jail machine.
	New func(r proc.Runner, machine string, opts common.Options) backend.Backend
	// JailPrefix is the argv prefix that runs a command INSIDE the jail for an
	// already-resolved identity — no EnsureUp state, no I/O.
	JailPrefix func(machine, user string) []string
}

// backends is the single table, in the order `lever backends` lists them;
// adding a backend means one entry here.
var backends = []entry{
	{
		Profile: orbstack.Profile,
		New: func(r proc.Runner, machine string, o common.Options) backend.Backend {
			return orbstack.New(r, machine, o)
		},
		JailPrefix: orbstack.JailPrefix,
	},
	{
		Profile:    lima.Profile,
		New:        func(r proc.Runner, machine string, o common.Options) backend.Backend { return lima.New(r, machine, o) },
		JailPrefix: func(machine, _ string) []string { return lima.JailPrefix(machine) },
	},
}

// ErrUnknownBackend is wrapped by every lookup of a backend name that is not
// registered; the message lists the valid names.
var ErrUnknownBackend = errors.New("unknown backend")

// lookup resolves a name (empty → Default) to its entry, or an error listing
// the valid set so a config can never silently run a different backend than it
// asked for.
func lookup(name string) (entry, error) {
	if name == "" {
		name = Default
	}
	e, ok := find(name)
	if !ok {
		return entry{}, fmt.Errorf("%w %q (valid: %s)", ErrUnknownBackend, name, strings.Join(Names(), ", "))
	}
	return e, nil
}

func find(name string) (entry, bool) {
	i := slices.IndexFunc(backends, func(e entry) bool { return e.Profile.Name == name })
	if i < 0 {
		return entry{}, false
	}
	return backends[i], true
}

// Names lists the selectable backend names, sorted.
func Names() []string {
	out := make([]string, 0, len(backends))
	for _, e := range backends {
		out = append(out, e.Profile.Name)
	}
	slices.Sort(out)
	return out
}

// Candidates lists every backend Lever can run with the guarantees it
// declares, in table order: the substrate guarantee matrix `lever backends`
// prints and config validation pins.
func Candidates() []backend.Profile {
	out := make([]backend.Profile, 0, len(backends))
	for _, e := range backends {
		out = append(out, e.Profile)
	}
	return out
}

// ProfileFor returns the declared guarantee profile for a backend name.
func ProfileFor(name string) (backend.Profile, bool) {
	e, ok := find(name)
	return e.Profile, ok
}

// Select builds the named backend for a jail machine. An empty name uses
// Default. The host-network escape hatch is read from the environment here
// (and in JailRunner) — the only two construction sites — so backends and
// transports stay pure.
func Select(name string, r proc.Runner, machine string) (backend.Backend, error) {
	e, err := lookup(name)
	if err != nil {
		return nil, err
	}
	return e.New(r, machine, common.Options{ForceHostNetwork: jail.ForceHostNetworkFromEnv()}), nil
}

// JailArgv returns the argv prefix that runs a command INSIDE the jail for the
// named backend, from an already-resolved identity. The remote proxy's
// jail-dial transport needs the bare prefix rather than a JailRunner: it execs
// `<prefix> nc host port` and adapts the child's pipes to a net.Conn, which
// proc.Runner's run-to-completion, capture-the-output contract cannot express.
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
func JailRunner(name string, host proc.Runner, machine, user, uid string) (proc.Runner, error) {
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
