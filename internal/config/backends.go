package config

import "slices"

// Backend names a config may select. The list lives here so a YAML load
// never links the runtime backends; backends_test.go keeps it in lockstep
// with internal/backend's Candidates.
const (
	BackendOrbstack = "orbstack"
	BackendLima     = "lima"
)

// KnownBackends lists every backend lever can run.
var KnownBackends = []string{BackendOrbstack, BackendLima}

func backendNames() []string {
	out := slices.Clone(KnownBackends)
	slices.Sort(out)
	return out
}

func knownBackend(name string) bool {
	return slices.Contains(KnownBackends, name)
}

// GuestLoginIssuerPort is the guest-side port the login forwarder listens
// on, and so the port in the hub's `issuer_url` (scion accepts only a
// loopback issuer).
//
// It is a constant, and not the port the host-side provider binds, because
// OrbStack mirrors a guest listener onto the host at the same number. With
// one number for both halves the provider could not bind, and a forwarder
// dialling host 8446 would dial the mirror of itself and hang. So the
// proxy's host listener (remote.port) may not name it. Nothing dials
// the mirror, so a second instance whose mirror cannot bind loses nothing.
const GuestLoginIssuerPort = 8446
