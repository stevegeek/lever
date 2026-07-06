package cli

import (
	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/backend/registry"
	"github.com/lever-to/lever/internal/exec"
	"github.com/spf13/cobra"
)

const Version = "0.3.0"

// BackendFactory builds a named backend for a given machine name.
type BackendFactory func(name, machine string) (backend.Backend, error)

// defaultFactory builds the named backend via the registry. Config validation
// guarantees a config's name is valid; flag-driven commands (provision, down,
// doctor with explicit --backend) surface registry errors directly.
func defaultFactory(name, machine string) (backend.Backend, error) {
	return registry.Select(name, exec.RealRunner{}, machine)
}

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Run: func(c *cobra.Command, _ []string) { c.Println(Version) }}
}

// NewHostRoot builds the host control-plane CLI (`lever`): provisioning only.
func NewHostRoot() *cobra.Command { return newHostRootWith(defaultFactory) }

// NewRootWithBackend is the host root with an injected backend (test seam).
func NewRootWithBackend(bf BackendFactory) *cobra.Command { return newHostRootWith(bf) }

func newHostRootWith(bf BackendFactory) *cobra.Command {
	root := &cobra.Command{Use: "lever", Short: "Jailed multi-agent orchestration (host control plane)"}
	root.AddCommand(versionCmd())
	root.AddCommand(newProvisionCmd(bf), newDestroyCmd(bf), newStopCmd(bf), newDoctorCmd(bf), newApplyCmd(bf), newUpCmd(bf), newReloadCmd(bf), newAttachCmd(bf), newHostMsgCmd(bf), newBrokerCmd(), newRevokeCmd(), newAcceptanceCmd(bf), newBackendsCmd(), newInitCmd())
	return root
}

// NewManagerRoot builds the in-jail orchestration CLI (`lever-manager`).
func NewManagerRoot() *cobra.Command { return newManagerRootWith() }

func newManagerRootWith() *cobra.Command {
	root := &cobra.Command{Use: "lever-manager", Short: "In-jail grove orchestration"}
	root.AddCommand(versionCmd())
	root.AddCommand(newAgentCmd(), newMsgCmd(), newWatchCmd())
	return root
}
