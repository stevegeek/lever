// Package types holds the plain data types the Backend interface carries for
// its guest-side verbs. It is a stdlib-only leaf so that internal/backend and
// internal/backend/guest can both name them without importing each other.
package types

// HubAuth is what the guest needs for the hub's proxy auth mode: lever signs
// an assertion per request with a host-held key, and the hub verifies it with
// the public half installed root-owned in the guest.
type HubAuth struct {
	// PublicKeyPEM is the PKIX PEM of lever's assertion key.
	PublicKeyPEM []byte
	// Issuer and Audience are the iss and aud the hub requires.
	Issuer   string
	Audience string
	// AdminEmail is the hub's only admin_emails entry.
	AdminEmail string
	// Header carries the assertion.
	Header string
	// RemoteOn keeps the web-only settings (the first-run display name and
	// native chat) converged on; off removes lever's display name.
	RemoteOn bool
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
