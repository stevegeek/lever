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

// backendNames lists the selectable backend names, sorted.
func backendNames() []string {
	out := slices.Clone(KnownBackends)
	slices.Sort(out)
	return out
}

// knownBackend reports whether name is a backend lever can run.
func knownBackend(name string) bool {
	return slices.Contains(KnownBackends, name)
}

// GuestLoginIssuerPort is the GUEST-side port of lever's remote-access login
// path: what the guest forwarder listens on, and therefore the port named in
// the hub's `issuer_url` (scion only accepts a loopback issuer, and this is
// that loopback, from the hub's point of view).
//
// It is a constant rather than a config key on purpose, and it is deliberately
// NOT the port the host-side provider binds. The container runtime mirrors a
// guest listener onto the host at the same number, so neither of lever's host
// listeners (remote.port, remote.login_port) may name it — validateRemote
// rejects both.
//
// OrbStack MIRRORS a guest listener onto the host at the same number: a
// forwarder on guest 127.0.0.1:8446 makes host 127.0.0.1:8446 an OrbStack
// listener too. When both halves used one number the host provider could not
// bind its own port — and worse, it was order-dependent, since a provider that
// bound first left OrbStack unable to mirror and the whole thing worked by
// luck. Two numbers, from two sources, is what removes that: the operator sets
// only the host one (remote.login_port), so no configuration can make the two
// halves of one instance collide.
//
// A bind collision was not even the worst of it. A forwarder that listens on
// guest 8446 and dials host 8446 dials the MIRROR OF ITSELF: guest 8446 ->
// host 8446 -> back to guest 8446. Live, that was not an error but a hang —
// the hub's discovery fetch never returned, `/auth/login/oidc` 500'd, and the
// browser got a 502 with every process apparently healthy. Whoever is tempted
// to simplify these back into one number should know that is the failure they
// would be restoring.
//
// The mirror at 8446 is therefore a host port lever reserves whenever remote
// access is on. Nothing dials it — the hub reaches the provider through the
// forwarder, and the forwarder dials the host by alias — so a second instance
// whose mirror cannot bind loses nothing.
//
// (This is the third defect from that mirroring, after the proxy reaching
// another instance's hub on host 127.0.0.1:8080. A host-side loopback address
// that appears to be a guest service is never what it seems.)
const GuestLoginIssuerPort = 8446
