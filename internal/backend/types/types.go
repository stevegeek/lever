// Package types holds the plain data types the Backend interface carries for
// its guest-side verbs. It is a stdlib-only leaf so that internal/backend (the
// contract) and internal/backend/guest (the helper that fills these in) can
// both name them without either importing the other.
package types

// HubLogin is what the guest needs in order to serve lever's remote-access
// login path: the two ports of the bridge, how the guest reaches the host, and
// the client_id the provider expects. The Backend interface and guest both name
// it from here, so neither package needs the other for the data.
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

// ScionProjectState is the scion project-registration state `lever doctor`
// inspects to catch the "registered but the in-tree marker is gone" corruption
// a bad teardown leaves behind (a bare container kill, rather than scion
// suspend/down). It is read-only: the fields come from files in the jail, not
// from talking to scion. The Backend interface and guest both name it
// from here.
type ScionProjectState struct {
	// MarkerPresent reports whether the in-tree marker (<MountDest>/.scion)
	// exists — scion's record, inside the bind-mounted tree, that the project is
	// initialized.
	MarkerPresent bool
	// Entries are scion's per-project registrations from the jail user's
	// ~/.scion/project-configs (one dir per project), each with the workspace
	// path it claims. Duplicates for one path, or an entry for the tree while
	// MarkerPresent is false, are the corruption doctor flags.
	Entries []ScionProjectEntry
}

// ScionProjectEntry is one ~/.scion/project-configs registration.
type ScionProjectEntry struct {
	Name          string // the project-configs directory name, e.g. "lever__c857bb16"
	WorkspacePath string // settings.yaml workspace_path, e.g. "/lever"
}
