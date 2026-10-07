package host

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
)

// newWorkerCmd is the host-side worker admin command. Distinct from the
// in-container `agent` command (which drives workers via the broker): `worker`
// runs host-side and reaches the scion runtime directly, like destroy/stop.
func newWorkerCmd(factory BackendFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "worker", Short: "Manage worker agents host-side"}
	cmd.AddCommand(newWorkerPurgeCmd(factory))
	return cmd
}

// newWorkerPurgeCmd deletes a worker's scion record and its staged ticket so
// the worker can be re-dispatched fresh with a NEW task (scion pins the task at
// creation, so a resume can only replay the original). It is the sanctioned
// teardown the worker path lacked — no hub-API surgery. It NEVER deletes the
// worker's HostWorkspace: that is its work product, and must survive a purge.
// Destructive, so it requires --force. Only configured worker names are accepted.
func newWorkerPurgeCmd(factory BackendFactory) *cobra.Command {
	var force bool
	var machine, backendFlag *string
	c := &cobra.Command{
		Use:   "purge NAME",
		Args:  cobra.ExactArgs(1),
		Short: "Delete a worker's scion record + staged ticket (keeps its work product)",
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !force {
				return fmt.Errorf("`lever worker purge %s` deletes the worker's scion record and staged ticket so it can run a new task (its work product in the workspace is KEPT); re-run with --force to proceed", name)
			}

			// Config (and its beside-the-config state dir) is discovered from the
			// CWD; the positional is the worker NAME, never a config path.
			app, state, err := loadAppAndState(nil)
			if err != nil {
				return err
			}

			_, b, err := resolveJailBackend(factory, *machine, *backendFlag)
			if err != nil {
				return err
			}
			// The ticket dir and the jail runner's XDG_RUNTIME_DIR both come
			// from the run user's uid; unread, it is the default (501), which
			// is wrong on Lima (guest uid 1000).
			if err := b.ResolveRunUser(cmd.Context()); err != nil {
				return fmt.Errorf("reading the jail run user: %w", err)
			}

			// Resolve the worker spec from config with the SAME derivation the
			// broker/apply use (brokerctl.WorkerSpecs), so HostWorkspace/TicketDir
			// match exactly — never a manager-supplied or ad-hoc path.
			spec, ok := findWorkerSpec(brokerctl.WorkerSpecs(app, b.MountDest(), b.RunUID()), name)
			if !ok {
				return fmt.Errorf("unknown worker %q — declare it under `workers:` in %s", name, config.CanonicalName)
			}

			// Delete the scion record via the same runtime seam newDestroyCmd
			// reaches through (a host-side scion client over the jail runner,
			// authenticating with the controller PAT), then the staged ticket
			// in the guest runtime dir. brokerctl.PurgeWorker is shared with
			// the broker's recycle route. HostWorkspace holds the worker's
			// work product and is never touched; nothing of the worker's
			// lives in the tree but that.
			sc := brokerctl.HostScionClient(b.JailRunner(), state, app.Scion.AgentRole)
			ticketErr, err := brokerctl.PurgeWorker(cmd.Context(), sc, b.JailRunner(), spec.Name, b.MountDest())
			if err != nil {
				return err
			}
			if ticketErr != nil {
				cmd.PrintErrf("warning: removing staged ticket for %q: %v\n", spec.Name, ticketErr)
			}

			cmd.Printf("worker %q purged — scion record deleted; work product in %s kept.\n", spec.Name, spec.HostWorkspace)
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "confirm the destructive purge (required)")
	machine, backendFlag = addJailTargetFlags(c)
	return c
}

// findWorkerSpec returns the spec whose Name matches, and whether one was found.
func findWorkerSpec(specs []broker.WorkerSpec, name string) (broker.WorkerSpec, bool) {
	for _, s := range specs {
		if s.Name == name {
			return s, true
		}
	}
	return broker.WorkerSpec{}, false
}
