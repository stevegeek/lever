package testutil

// ManagerObservation is one way the hub can show the manager's record: its
// phase and the container status it reports. An empty Phase is no record.
type ManagerObservation struct {
	Name, Phase, Container string
}

// ManagerObservations is the shared fake observation set the lifecycle
// characterization tables run every entry point over (start-manager, up,
// conversation new and restore), so a decision one entry point makes and
// another does not shows up side by side. It holds every phase lever's code
// tells apart, the container statuses the liveness checks read, and a phase
// lever does not know.
var ManagerObservations = []ManagerObservation{
	{"absent", "", ""},
	{"running", "running", "Up 6 seconds"},
	{"running-exited", "running", "Exited (1) 2 seconds ago"},
	{"running-created", "running", "created"},
	{"running-no-status", "running", ""},
	{"suspended", "suspended", "stopped"},
	{"stopped", "stopped", "stopped"},
	{"error", "error", "Exited (137) 1 minute ago"},
	{"created", "created", "created"},
	{"provisioning", "provisioning", ""},
	{"cloning", "cloning", ""},
	{"starting", "starting", ""},
	{"stopping", "stopping", "Up 2 minutes"},
	{"resumed", "resumed", "Up 1 second"},
	{"unknown", "definitely-not-a-phase", ""},
}
