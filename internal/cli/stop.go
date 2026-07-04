package cli

import (
	"path/filepath"

	"github.com/lever-to/lever/internal/brokerctl"
	"github.com/lever-to/lever/internal/config"
	"github.com/lever-to/lever/internal/scion"
	"github.com/spf13/cobra"
)

// newStopCmd powers the jail machine off while keeping its disk, so a
// following `lever up` can resume fast (no re-apply, no reinstall). This is
// distinct from `destroy`, which deletes the machine and clears staged
// runtime state.
func newStopCmd(factory BackendFactory) *cobra.Command {
	var machine, backendFlag string
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Power off the jail, keeping its disk (fast `lever up` resume)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Resolve the app (for the manager's scion slug) and, when targeting
			// the current instance (no explicit --machine), stop the host-side
			// broker too — mirroring `destroy`'s broker-stop block. UNLIKE
			// destroy, staged runtime state (bootstrap ticket, manifest) is left
			// alone: stop preserves everything for a fast resume.
			var appName string
			if path, perr := resolveConfigPath(""); perr == nil {
				if app, lerr := config.Load(path); lerr == nil {
					appName = app.Name
					if machine == "" {
						if serr := brokerctl.StateDir(filepath.Dir(path)).StopBroker(); serr != nil {
							cmd.PrintErrf("warning: stopping broker: %v\n", serr)
						}
					}
				}
			}
			if machine != "" {
				cmd.PrintErrln("note: --machine given; the broker is not stopped (run `lever stop` from the instance root to do that).")
			}

			m, err := machineFromFlagOrConfig(machine)
			if err != nil {
				return err
			}
			b, err := factory(backendFromFlagOrConfig(backendFlag), m)
			if err != nil {
				return err
			}

			// Best-effort clean stop: STOP (not suspend) the manager first, but
			// only when the jail is actually reachable — a halted (or never-up)
			// machine must still be stoppable, so a ResolveRunUser failure just
			// skips this step rather than failing the command. The manager's
			// conversation cannot survive the VM power-off below regardless — an
			// in-memory process is simply gone once the VM is off — so suspending
			// it would only persist an un-resumable "suspended" hub.db record: the
			// next `lever up` would try to RESUME that record via `scion start`
			// and fail with scion's 500 "cannot resume ... agent does not exist".
			// Stopping instead discards the session cleanly, so the next `up`
			// starts a fresh manager rather than trying (and failing) to resume a
			// dead one. Any Stop error itself is ignored: the VM is about to power
			// off anyway. NOTE this does NOT affect DETACH->up resume: `lever
			// detach` suspends via scion-attach's own path with the VM still UP
			// (a real, resumable suspend); this best-effort call only covers the
			// power-off path (`lever stop`).
			if appName != "" {
				if err := b.ResolveRunUser(cmd.Context()); err == nil {
					sc := scion.New(b.JailRunner(), scion.Options{HubEndpoint: "http://127.0.0.1:8080"})
					_ = sc.Stop(cmd.Context(), appName, b.MountDest())
				}
			}

			if err := b.Stop(cmd.Context()); err != nil {
				return err
			}
			cmd.Printf("machine %q stopped — disk preserved; run `lever up` to resume.\n", m)
			return nil
		},
	}
	cmd.Flags().StringVar(&machine, "machine", "", "jail machine name (default: lever-<name> from config)")
	cmd.Flags().StringVar(&backendFlag, "backend", "", "containment backend (default: config's backend, else the registry default)")
	return cmd
}
