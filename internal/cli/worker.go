package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/scion"
)

// newWorkerCmd is the host-side worker admin command. Distinct from the
// in-container `agent` command (which drives workers via the broker): `worker`
// runs host-side and reaches the scion runtime directly, like destroy/stop.
func newWorkerCmd(factory BackendFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "worker", Short: "Manage worker agents host-side"}
	cmd.AddCommand(newWorkerPurgeCmd(factory))
	return cmd
}

// newWorkerPurgeCmd deletes a worker's scion record and its staged bootstrap so
// the worker can be re-dispatched fresh with a NEW task (scion pins the task at
// creation, so a resume can only replay the original). It is the sanctioned
// teardown the worker path lacked — no hub-API surgery. It NEVER deletes the
// worker's HostWorkspace: that is its work product, and must survive a purge.
// Destructive, so it requires --force. Only configured worker names are accepted.
func newWorkerPurgeCmd(factory BackendFactory) *cobra.Command {
	var force bool
	var machine, backendFlag string
	c := &cobra.Command{
		Use:   "purge NAME",
		Args:  cobra.ExactArgs(1),
		Short: "Delete a worker's scion record + staged bootstrap (keeps its work product)",
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !force {
				return fmt.Errorf("`lever worker purge %s` deletes the worker's scion record and staged bootstrap so it can run a new task (its work product in the workspace is KEPT); re-run with --force to proceed", name)
			}

			path, err := resolveConfigPath("")
			if err != nil {
				return err
			}
			app, err := config.Load(path)
			if err != nil {
				return err
			}

			m, err := machineFromFlagOrConfig(machine)
			if err != nil {
				return err
			}
			b, err := factory(backendFromFlagOrConfig(backendFlag), m)
			if err != nil {
				return err
			}

			// Resolve the worker spec from config with the SAME derivation the
			// broker/apply use (brokerctl.WorkerSpecs), so HostWorkspace/BootstrapDir
			// match exactly — never a manager-supplied or ad-hoc path.
			spec, ok := findWorkerSpec(brokerctl.WorkerSpecs(app, b.MountDest()), name)
			if !ok {
				return fmt.Errorf("unknown worker %q — declare it under `workers:` in %s", name, filepath.Base(path))
			}

			// Delete the scion record via the same runtime seam newDestroyCmd/
			// restartManagerFresh reach through: a host-side scion client over the
			// jail runner, authenticating with the controller PAT.
			project := b.MountDest()
			state := brokerctl.StateDir(filepath.Dir(path))
			sc := scion.New(b.JailRunner(), scion.Options{
				HubEndpoint:    "http://127.0.0.1:8080",
				HubTokenSource: func() string { t, _ := state.LoadControllerPAT(); return t },
			})
			if err := sc.Delete(cmd.Context(), spec.Name, project); err != nil {
				return fmt.Errorf("deleting worker %q scion record: %w", spec.Name, err)
			}

			// Remove the staged bootstrap (a spent, worker-specific ticket) — but
			// ONLY that file (and the .lever dir if now empty). HostWorkspace, which
			// the bootstrap dir lives inside, holds the worker's work product and is
			// never touched.
			bootstrap := filepath.Join(spec.BootstrapDir, "bootstrap.json")
			if err := os.Remove(bootstrap); err != nil && !os.IsNotExist(err) {
				cmd.PrintErrf("warning: removing staged bootstrap %s: %v\n", bootstrap, err)
			}
			_ = os.Remove(spec.BootstrapDir) // removed only if now empty

			cmd.Printf("worker %q purged — scion record deleted; work product in %s kept.\n", spec.Name, spec.HostWorkspace)
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "confirm the destructive purge (required)")
	c.Flags().StringVar(&machine, "machine", "", "jail machine name (default: lever-<name> from config)")
	c.Flags().StringVar(&backendFlag, "backend", "", "containment backend (default: config's backend, else the registry default)")
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
