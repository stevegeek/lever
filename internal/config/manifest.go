package config

// ManifestName is the filename of the legacy sanitized runtime manifest the host
// used to write INTO the mount at apply time. The file is no longer written by
// `lever apply` (Task 9: manifest removed as write-only dead code). The const is
// retained so `lever down` can remove any legacy manifest left from a prior
// version (see internal/cli/down.go clearStagedRuntimeState).
const ManifestName = ".lever-manifest.yaml"
