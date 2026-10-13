// Package types holds the plain data types the Backend interface carries for
// its guest-side verbs. It is a stdlib-only leaf so that internal/backend and
// internal/backend/guest can both name them without importing each other.
package types

// HubLogin is what the guest needs in order to serve lever's remote-access
// login path.
type HubLogin struct {
	// IssuerPort is the port the forwarder listens on INSIDE the guest, and
	// the one the hub's issuer_url names. Always config.GuestLoginIssuerPort
	// in production; a field so tests can vary it.
	IssuerPort int
	// HostPort is the port the provider binds ON THE HOST, and the one the
	// forwarder dials there. Never equal to IssuerPort; see
	// config.GuestLoginIssuerPort.
	HostPort int
	// HostAddress is how the GUEST reaches the host, e.g. "host.orb.internal"
	// or its resolved IPv4.
	HostAddress string
	// ClientID is the client_id lever's provider expects
	// (remoteproxy.LoginClientID).
	ClientID string
}

// ScionProjectState is the scion project-registration state `lever doctor`
// inspects to catch the "registered but the in-tree marker is gone" corruption
// a bad teardown leaves behind (a bare container kill, rather than scion
// suspend/down). The fields come from files in the jail, not from scion.
type ScionProjectState struct {
	// MarkerPresent reports whether the in-tree marker (<MountDest>/.scion)
	// exists.
	MarkerPresent bool
	// Entries are the registrations in the jail user's
	// ~/.scion/project-configs. Duplicates for one path, or an entry for the
	// tree while MarkerPresent is false, are the corruption doctor flags.
	Entries []ScionProjectEntry
}

// ScionProjectEntry is one ~/.scion/project-configs registration.
type ScionProjectEntry struct {
	Name          string // the project-configs directory name, e.g. "lever__c857bb16"
	WorkspacePath string // settings.yaml workspace_path, e.g. "/lever"
}
