package backend

import "sort"

// Status is how far a backend has progressed. Only Implemented backends can be
// selected by a config; the others declare their TARGET guarantees so the
// substrate roadmap is visible (via `lever backends`) without pretending an
// unbuilt backend already enforces them.
type Status string

const (
	StatusImplemented  Status = "implemented"
	StatusPlanned      Status = "planned"
	StatusExperimental Status = "experimental"
)

// Candidate is one containment backend and the guarantees it declares. This
// slice is the SINGLE SOURCE of the substrate guarantee matrix: config
// validation, the `lever backends` command, and the reference docs all read it,
// so the tool and the docs cannot drift. A concrete backend's Profile() returns
// ProfileFor(its name), so the declared guarantee and the runtime one are the
// same value.
type Candidate struct {
	Name    string
	Status  Status
	Profile Profile
	// Note is a one-line human summary shown by `lever backends`.
	Note string
}

// Candidates lists every backend Lever knows about — built or roadmap. The one
// with a constructor (see internal/backend/registry) is orbstack; the rest carry
// TARGET profiles clearly marked by Status and are rejected at config-load.
var Candidates = []Candidate{
	{
		Name:   "orbstack",
		Status: StatusImplemented,
		Profile: Profile{
			Name:             "orbstack",
			SeparateKernel:   false, // shares the one OrbStack VM kernel across manager+groves
			FSBoundedBy:      "isolated machine: no host files + project tree mounted at /lever",
			EgressEnforcedAt: "jail netns iptables/ip6tables",
			VersionFragile:   true, // depends on OrbStack --isolated behaviours
		},
		Note: "reference backend; macOS + Apple Silicon; the validated substrate today",
	},
	{
		Name:   "linux-docker",
		Status: StatusPlanned,
		Profile: Profile{
			Name:             "linux-docker",
			SeparateKernel:   false, // native host kernel — no hypervisor boundary
			FSBoundedBy:      "host netns+userns + single bind mount",
			EgressEnforcedAt: "jail netns nftables/iptables",
			VersionFragile:   false, // built from stable kernel primitives, not vendor behaviours
		},
		Note: "native Linux, no VM: strongest FS/egress and no virtiofs tax, but shares the host kernel",
	},
	{
		Name:   "lima",
		Status: StatusPlanned,
		Profile: Profile{
			Name:             "lima",
			SeparateKernel:   true, // own VM kernel, like OrbStack
			FSBoundedBy:      "VM: no host files + project tree mounted",
			EgressEnforcedAt: "jail netns iptables/ip6tables",
			VersionFragile:   true,
		},
		Note: "the OrbStack-equivalent VM jail for macOS/Linux users who don't run OrbStack",
	},
	{
		Name:   "apple-container",
		Status: StatusExperimental,
		Profile: Profile{
			Name:             "apple-container",
			SeparateKernel:   true, // a VM kernel PER agent — strongest isolation
			FSBoundedBy:      "per-agent VM: no host files + tree mounted",
			EgressEnforcedAt: "per-VM / gateway",
			VersionFragile:   true, // young; full networking needs macOS 26
		},
		Note: "per-agent micro-VM (different topology); most isolated Mac option, immature, needs macOS 26",
	},
}

// Lookup returns the candidate with the given name.
func Lookup(name string) (Candidate, bool) {
	for _, c := range Candidates {
		if c.Name == name {
			return c, true
		}
	}
	return Candidate{}, false
}

// ProfileFor returns the declared guarantee profile for a backend name.
func ProfileFor(name string) (Profile, bool) {
	c, ok := Lookup(name)
	return c.Profile, ok
}

// IsSelectable reports whether a config may select this backend (implemented only).
func IsSelectable(name string) bool {
	c, ok := Lookup(name)
	return ok && c.Status == StatusImplemented
}

// SelectableNames lists the backends a config may select, sorted.
func SelectableNames() []string {
	var out []string
	for _, c := range Candidates {
		if c.Status == StatusImplemented {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}
