package backend

// ScionProjectState is the scion project-registration state `lever doctor`
// inspects to catch the "registered but the in-tree marker is gone" corruption
// a bad teardown leaves behind (a bare container kill, rather than scion
// suspend/down). It is read-only: the fields come from files in the jail, not
// from talking to scion.
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
