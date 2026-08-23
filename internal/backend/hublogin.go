package backend

// HubLogin is what the guest needs in order to serve lever's remote-access
// login path: the two ports of the bridge, how the guest reaches the host, and
// the client_id the provider expects.
//
// It lives here rather than in package guest because the Backend interface
// carries it: the contract owns its types, and guest (an implementation
// helper) imports the contract, never the other way round — the same reason
// ScionProjectState lives here. See the package doc.
type HubLogin struct {
	// IssuerPort is the port the forwarder listens on INSIDE the guest, and
	// the one the hub's issuer_url names. Always config.GuestLoginIssuerPort
	// in production; a field rather than a direct reference to the constant so
	// tests can vary it.
	IssuerPort int
	// HostPort is the port the provider binds ON THE HOST, and the one the
	// forwarder dials there. Operator-settable (remote.login_port), because
	// it shares the host's loopback space with the broker and the proxy.
	// Never equal to IssuerPort — see config.GuestLoginIssuerPort.
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
