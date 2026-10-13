package config

import "slices"

// Backend names a config may select. The list lives here so a YAML load
// never links the runtime backends; backends_test.go keeps it in lockstep
// with internal/backend's Candidates.
const (
	BackendOrbstack = "orbstack"
	BackendLima     = "lima"
)

// KnownBackends lists every backend lever can run.
var KnownBackends = []string{BackendOrbstack, BackendLima}

func backendNames() []string {
	out := slices.Clone(KnownBackends)
	slices.Sort(out)
	return out
}

func knownBackend(name string) bool {
	return slices.Contains(KnownBackends, name)
}
