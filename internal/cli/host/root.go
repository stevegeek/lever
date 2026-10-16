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

type BackendFactory func(name, machine string) (backend.Backend, error)

// defaultFactory builds the named backend via the registry; a flag-supplied
// name surfaces registry errors directly.
func defaultFactory(name, machine string) (backend.Backend, error) {
	return registry.Select(name, proc.Traced(proc.RealRunner{}), machine)
}

// NewRoot builds the host control-plane CLI (`lever`).
func NewRoot() *cobra.Command { return newRootWith(defaultFactory) }

func newRootWith(bf BackendFactory) *cobra.Command {
	// SilenceErrors: the binary prints a returned error itself, sanitized (see
	// cli.Execute). Usage is printed for flag and argument errors only.
	root := &cobra.Command{Use: "lever", Short: "Jailed multi-agent orchestration (host control plane)", SilenceErrors: true,
		Version: cli.VersionString()}
	root.SetVersionTemplate("{{.Version}}\n")
	root.AddCommand(cli.VersionCmd())
	root.AddCommand(newProvisionCmd(bf), newDestroyCmd(bf), newStopCmd(bf), newDoctorCmd(bf), newApplyCmd(bf), newUpCmd(bf), newReloadCmd(bf), newAttachCmd(bf), newSSHCmd(bf), newSSHConfigCmd(), newHostMsgCmd(bf), newBrokerCmd(), newRevokeCmd(), newAcceptanceCmd(bf), newBackendsCmd(), newInitCmd(), newDirectiveCmd(), newWorkerCmd(bf), newRemoteCmd(bf), newVoiceCmd(), newConversationCmd(bf), newTreeCmd(bf))
	silenceUsageOnRunErrors(root)
	return root
}

// usageError marks a RunE error for a misuse cobra cannot check itself (a flag
// combination); the usage text follows it.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

// silenceUsageOnRunErrors wraps every RunE under c so a returned error
// silences cobra's usage text unless it is a usageError; otherwise a runtime
// failure reads as a flag problem. Cobra's own flag and argument checks run
// before RunE and keep it.
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
