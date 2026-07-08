package backend

import "sort"

// Candidate is one containment backend and the guarantees it declares. This
// slice is the SINGLE SOURCE of the substrate guarantee matrix — and it lists
// ONLY implemented backends: a candidate exists iff internal/backend/registry
// has a constructor for it (enforced by the registry's lockstep test). Roadmap
// and rejected backends are documentation, not code — see
// docs-site/_reference/backends.md, which also states the contract's guarantee
// 0: a hypervisor boundary between the agent workload and the host kernel is
// mandatory; no backend without it may be added here.
type Candidate struct {
	Name    string
	Profile Profile
	// Note is a one-line human summary shown by `lever backends`.
	Note string
}

// Candidates lists every backend Lever can run.
var Candidates = []Candidate{
	{
		Name: "orbstack",
		Profile: Profile{
			Name:             "orbstack",
			SeparateKernel:   false, // shares the one OrbStack VM kernel across manager+workers
			FSBoundedBy:      "isolated machine: no host files + project tree mounted at /lever",
			EgressEnforcedAt: "jail netns iptables/ip6tables",
			VersionFragile:   true, // depends on OrbStack --isolated behaviours
		},
		Note: "reference backend; macOS + Apple Silicon; the validated substrate today",
	},
	{
		Name: "lima",
		Profile: Profile{
			Name:             "lima",
			SeparateKernel:   true, // own Lima VM kernel, not shared with the host or other jails
			FSBoundedBy:      "VM: no host files + project tree mounted at /lever",
			EgressEnforcedAt: "jail netns iptables/ip6tables",
			VersionFragile:   true, // depends on Lima's portForwards/mount behaviours
		},
		Note: "VM jail via Lima: macOS (vz) and Linux (QEMU/KVM); the non-OrbStack path",
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

// Names lists the selectable backend names, sorted.
func Names() []string {
	var out []string
	for _, c := range Candidates {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}
