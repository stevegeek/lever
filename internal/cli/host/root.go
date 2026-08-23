// Package host implements the `lever` binary: the host control plane that
// provisions machines, runs the broker and remote proxy, and drives the
// agent container from the operator's side of the trust boundary. Nothing
// here runs inside the container.
package host

import (
	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/registry"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/exec"
)

// BackendFactory builds a named backend for a given machine name.
type BackendFactory func(name, machine string) (backend.Backend, error)

// defaultFactory builds the named backend via the registry. Config validation
// guarantees a config's name is valid; flag-driven commands (provision, down,
// doctor with explicit --backend) surface registry errors directly.
func defaultFactory(name, machine string) (backend.Backend, error) {
	return registry.Select(name, exec.RealRunner{}, machine)
}

// NewRoot builds the host control-plane CLI (`lever`): provisioning only.
func NewRoot() *cobra.Command { return newRootWith(defaultFactory) }

func newRootWith(bf BackendFactory) *cobra.Command {
	root := &cobra.Command{Use: "lever", Short: "Jailed multi-agent orchestration (host control plane)"}
	root.AddCommand(cli.VersionCmd())
	root.AddCommand(newProvisionCmd(bf), newDestroyCmd(bf), newStopCmd(bf), newDoctorCmd(bf), newApplyCmd(bf), newUpCmd(bf), newReloadCmd(bf), newAttachCmd(bf), newHostMsgCmd(bf), newBrokerCmd(), newRevokeCmd(), newAcceptanceCmd(bf), newBackendsCmd(), newInitCmd(), newDirectiveCmd(), newWorkerCmd(bf), newRemoteCmd(bf))
	return root
}
