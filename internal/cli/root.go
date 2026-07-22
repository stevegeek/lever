package cli

import (
	"runtime/debug"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/registry"
	"github.com/stevegeek/lever/internal/exec"
)

const Version = "0.9.2"

// BackendFactory builds a named backend for a given machine name.
type BackendFactory func(name, machine string) (backend.Backend, error)

// defaultFactory builds the named backend via the registry. Config validation
// guarantees a config's name is valid; flag-driven commands (provision, down,
// doctor with explicit --backend) surface registry errors directly.
func defaultFactory(name, machine string) (backend.Backend, error) {
	return registry.Select(name, exec.RealRunner{}, machine)
}

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Run: func(c *cobra.Command, _ []string) { c.Println(versionString()) }}
}

// versionString augments the hardcoded release Version with Go's embedded VCS
// stamp when present: the commit the binary was built from (short) plus a
// "-dirty" marker for an uncommitted tree, or — for a `go install module@vX`
// build, which carries no VCS stamp — the module version. This stops `lever
// version` from masking a stale or local build behind the bare release string
// (a make-install binary can lag the source it was built from, which the
// hardcoded const alone hides).
func versionString() string {
	var rev, modVersion string
	dirty := false
	if info, ok := debug.ReadBuildInfo(); ok {
		modVersion = info.Main.Version
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	return formatVersion(Version, rev, dirty, modVersion)
}

// formatVersion renders the version line from the release string plus whichever
// build provenance is available: a VCS commit (local builds) takes precedence
// over a module version (go install builds); with neither, just the release.
func formatVersion(base, rev string, dirty bool, modVersion string) string {
	switch {
	case rev != "":
		short := rev
		if len(short) > 12 {
			short = short[:12]
		}
		if dirty {
			short += "-dirty"
		}
		return base + " (" + short + ")"
	case modVersion != "" && modVersion != "(devel)":
		return base + " (" + modVersion + ")"
	default:
		return base
	}
}

// NewHostRoot builds the host control-plane CLI (`lever`): provisioning only.
func NewHostRoot() *cobra.Command { return newHostRootWith(defaultFactory) }

// NewRootWithBackend is the host root with an injected backend (test seam).
func NewRootWithBackend(bf BackendFactory) *cobra.Command { return newHostRootWith(bf) }

func newHostRootWith(bf BackendFactory) *cobra.Command {
	root := &cobra.Command{Use: "lever", Short: "Jailed multi-agent orchestration (host control plane)"}
	root.AddCommand(versionCmd())
	root.AddCommand(newProvisionCmd(bf), newDestroyCmd(bf), newStopCmd(bf), newDoctorCmd(bf), newApplyCmd(bf), newUpCmd(bf), newReloadCmd(bf), newAttachCmd(bf), newHostMsgCmd(bf), newBrokerCmd(), newRevokeCmd(), newAcceptanceCmd(bf), newBackendsCmd(), newInitCmd(), newDirectiveCmd(), newWorkerCmd(bf))
	return root
}

// NewManagerRoot builds the in-jail orchestration CLI (`lever-manager`).
func NewManagerRoot() *cobra.Command { return newManagerRootWith() }

func newManagerRootWith() *cobra.Command {
	root := &cobra.Command{Use: "lever-manager", Short: "In-jail worker orchestration"}
	root.AddCommand(versionCmd())
	root.AddCommand(newAgentCmd(), newMsgCmd(), newWatchCmd())
	return root
}
