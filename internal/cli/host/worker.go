package host

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
)

// newWorkerCmd is the host-side worker admin command; unlike the in-container
// `agent` command it reaches the scion runtime directly.
func newWorkerCmd(factory BackendFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "worker", Short: "Manage worker agents host-side"}
	cmd.AddCommand(newWorkerPurgeCmd(factory), newWorkerListCmd(), newWorkerStartCmd(), newWorkerResumeCmd(),
		newWorkerRecycleCmd(), newWorkerSuspendCmd(), newWorkerStopCmd())
	return cmd
}

// newWorkerPurgeCmd deletes a worker's scion record and staged ticket so it
// can be dispatched fresh with a new task (scion pins the task at creation).
// It never deletes the worker's HostWorkspace, its work product. Destructive,
// so it requires --force; only configured worker names are accepted.
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

			// Config comes from the CWD; the positional is the worker name.
			app, state, err := loadAppAndState(nil)
			if err != nil {
				return err
			}

			_, b, err := resolveJailBackend(factory, *machine, *backendFlag)
			if err != nil {
				return err
			}
			// The ticket dir and the jail runner's XDG_RUNTIME_DIR come from the run
			// user's uid; the default (501) is wrong on Lima (1000).
			if err := b.ResolveRunUser(cmd.Context()); err != nil {
				return fmt.Errorf("reading the jail run user: %w", err)
			}

			// The spec comes from config by the broker's own derivation
			// (brokerctl.WorkerSpecs), never a manager-supplied path.
			spec, ok := findWorkerSpec(brokerctl.WorkerSpecs(app, b.MountDest(), b.RunUID()), name)
			if !ok {
				return fmt.Errorf("unknown worker %q — declare it under `workers:` in %s", name, config.CanonicalName)
			}

			// brokerctl.PurgeWorker (shared with the broker's recycle route) deletes the
			// record through a host-side scion client with the controller PAT, then the
			// ticket in the guest runtime dir. HostWorkspace is never touched.
			sc := brokerctl.HostScionClient(b.JailRunner(), state, app.Scion.AgentRole)
			ticketErr, err := brokerctl.PurgeWorker(cmd.Context(), sc, b.JailRunner(), spec.Name, b.MountDest())
			if err != nil {
				return err
			}
			if ticketErr != nil {
				cmd.PrintErrf("lever: warning: removing staged ticket for %q: %v\n", spec.Name, ticketErr)
			}

			cmd.Printf("worker %q purged — scion record deleted; work product in %s kept.\n", spec.Name, spec.HostWorkspace)
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "confirm the destructive purge (required)")
	machine, backendFlag = addJailTargetFlags(c)
	return c
}

func findWorkerSpec(specs []broker.WorkerSpec, name string) (broker.WorkerSpec, bool) {
	for _, s := range specs {
		if s.Name == name {
			return s, true
		}
	}
	return broker.WorkerSpec{}, false
}
