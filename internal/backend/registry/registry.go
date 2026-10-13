// Package registry constructs the selected containment backend and its jail
// transport. It is the one place that names a concrete backend.
//
// A hypervisor boundary between the agent workload and the host kernel is
// mandatory: no backend without it may be added here (see
// docs-site/_reference/backends.md).
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

// Default is the backend used when a caller supplies no name.
const Default = "orbstack"

type entry struct {
	// Profile is the guarantees the backend declares; its Name is the key.
	Profile backend.Profile
	New     func(r proc.Runner, machine string, opts common.Options) backend.Backend
	// JailPrefix must need no EnsureUp state and do no I/O.
	JailPrefix func(machine, user string) []string
}

// backends is in the order `lever backends` lists them.
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

// lookup errors on an unknown name so a config can never silently run a
// different backend than it asked for.
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

// Candidates lists every backend's declared guarantees, in table order.
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
// and in JailRunner, the only construction sites, so backends and transports
// stay pure.
func Select(name string, r proc.Runner, machine string) (backend.Backend, error) {
	e, err := lookup(name)
	if err != nil {
		return nil, err
	}
	return e.New(r, machine, common.Options{ForceHostNetwork: jail.ForceHostNetworkFromEnv()}), nil
}

// JailArgv returns the argv prefix that runs a command INSIDE the jail for the
// named backend, from an already-resolved identity. For callers that need a
// streaming child (e.g. `<prefix> nc host port` as a net.Conn), which
// proc.Runner's run-to-completion contract cannot express.
func JailArgv(name, machine, user string) ([]string, error) {
	e, err := lookup(name)
	if err != nil {
		return nil, err
	}
	return e.JailPrefix(machine, user), nil
}

// JailRunner rebuilds the command transport into a jail from its already-
// resolved identity (machine, run user, uid) WITHOUT needing EnsureUp state.
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
