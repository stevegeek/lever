// Package host implements the `lever` binary: the host control plane that
// provisions machines, runs the broker and remote proxy, and drives the
// agent container from the operator's side of the trust boundary. Nothing
// here runs inside the container.
package host

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/registry"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/proc"
)

// BackendFactory builds a named backend for a given machine name.
type BackendFactory func(name, machine string) (backend.Backend, error)

// defaultFactory builds the named backend via the registry. Config validation
// guarantees a config's name is valid; flag-driven commands (provision, down,
// doctor with explicit --backend) surface registry errors directly.
func defaultFactory(name, machine string) (backend.Backend, error) {
	return registry.Select(name, proc.Traced(proc.RealRunner{}), machine)
}

// NewRoot builds the host control-plane CLI (`lever`): provisioning only.
func NewRoot() *cobra.Command { return newRootWith(defaultFactory) }

func newRootWith(bf BackendFactory) *cobra.Command {
	// SilenceErrors: the binary prints a returned error itself, sanitized —
	// see cli.Execute. Usage is printed for flag and argument errors only
	// (silenceUsageOnRunErrors).
	// Version gives the root a --version flag that prints what `lever
	// version` prints.
	root := &cobra.Command{Use: "lever", Short: "Jailed multi-agent orchestration (host control plane)", SilenceErrors: true,
		Version: cli.VersionString()}
	root.SetVersionTemplate("{{.Version}}\n")
	root.AddCommand(cli.VersionCmd())
	root.AddCommand(newProvisionCmd(bf), newDestroyCmd(bf), newStopCmd(bf), newDoctorCmd(bf), newApplyCmd(bf), newUpCmd(bf), newReloadCmd(bf), newAttachCmd(bf), newHostMsgCmd(bf), newBrokerCmd(), newRevokeCmd(), newAcceptanceCmd(bf), newBackendsCmd(), newInitCmd(), newDirectiveCmd(), newWorkerCmd(bf), newRemoteCmd(bf), newVoiceCmd(), newConversationCmd(bf))
	silenceUsageOnRunErrors(root)
	return root
}

// usageError marks an error a command's RunE returns for a wrong use of its
// flags or arguments, which cobra cannot check itself (a combination of
// flags, say): the usage text follows it, as it follows cobra's own flag and
// argument errors.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

// silenceUsageOnRunErrors keeps cobra's usage text for the errors it is
// for. Cobra prints it after any error a command returns unless the command
// sets SilenceUsage, so a runtime failure (a manager that did not come up,
// lever#202) used to read as a flag problem. Every RunE under c is wrapped:
// an error it returns silences the usage text, unless it is a usageError.
// Cobra's own checks (an unknown flag, a missing or mutually exclusive
// flag, the wrong number of arguments) run before RunE and keep it.
func silenceUsageOnRunErrors(c *cobra.Command) {
	if run := c.RunE; run != nil {
		c.RunE = func(cmd *cobra.Command, args []string) error {
			err := run(cmd, args)
			var ue usageError
			if err != nil && !errors.As(err, &ue) {
				cmd.SilenceUsage = true
			}
			return err
		}
	}
	for _, sub := range c.Commands() {
		silenceUsageOnRunErrors(sub)
	}
}
