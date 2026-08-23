package config

import "slices"

// Backend names a config may select. The schema layer owns the list so a
// YAML load never links the runtime backends; internal/backend's Candidates
// table is held in lockstep by backends_test.go (package config_test).
const (
	BackendOrbstack = "orbstack"
	BackendLima     = "lima"
)

// KnownBackends lists every backend lever can run, in declaration order.
// Implemented backends only — roadmap/rejected backends are documentation.
var KnownBackends = []string{BackendOrbstack, BackendLima}

// BackendNames lists the selectable backend names, sorted.
func BackendNames() []string {
	out := slices.Clone(KnownBackends)
	slices.Sort(out)
	return out
}

// KnownBackend reports whether name is a backend lever can run.
func KnownBackend(name string) bool {
	return slices.Contains(KnownBackends, name)
}

// GuestLoginIssuerPort is the GUEST-side port of lever's remote-access login
// forwarder (backend.GuestLoginIssuerPort). The container runtime mirrors a
// guest listener onto the host at the same number, so neither of lever's
// host listeners (remote.port, remote.login_port) may name it — validateRemote
// rejects both.
const GuestLoginIssuerPort = 8446
