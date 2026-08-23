package backend

import "github.com/stevegeek/lever/internal/config"

// GuestLoginIssuerPort is the GUEST-side port of lever's remote-access login
// path: what the guest forwarder listens on, and therefore the port named in
// the hub's `issuer_url` (scion only accepts a loopback issuer, and this is
// that loopback, from the hub's point of view).
//
// It is a constant rather than a config key on purpose, and it is deliberately
// NOT the port the host-side provider binds.
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
const GuestLoginIssuerPort = config.GuestLoginIssuerPort

// HubLogin is what the guest needs in order to serve lever's remote-access
// login path: the two ports of the bridge, how the guest reaches the host, and
// the client_id the provider expects.
//
// It lives here rather than in package guest because the Backend interface
// carries it and guest imports backend (never the other way round) — the same
// reason ScionProjectState lives here.
type HubLogin struct {
	// IssuerPort is the port the forwarder listens on INSIDE the guest, and
	// the one the hub's issuer_url names. Always GuestLoginIssuerPort in
	// production; a field rather than a direct reference to the constant so
	// tests can vary it.
	IssuerPort int
	// HostPort is the port the provider binds ON THE HOST, and the one the
	// forwarder dials there. Operator-settable (remote.login_port), because
	// it shares the host's loopback space with the broker and the proxy.
	// Never equal to IssuerPort — see GuestLoginIssuerPort.
	HostPort int
	// HostAddress is how the GUEST reaches the host, e.g. "host.orb.internal"
	// or its resolved IPv4 — the same alias lever's agents already use to
	// reach the host broker.
	HostAddress string
	// ClientID is the client_id lever's provider expects
	// (remoteproxy.LoginClientID). Named rather than derived, so both ends of
	// the contract are explicit.
	ClientID string
}
