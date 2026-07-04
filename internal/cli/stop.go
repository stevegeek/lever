package cli

import (
	"context"
	"path/filepath"
	"time"

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

			// Best-effort checkpoint: SUSPEND the manager before power-off. The
			// conversation is durable — it lives in the agent home (persistent
			// bind-mount), and scion resume relaunches the harness with
			// `claude --continue`, restoring the session (live-proven 2026-07-04)
			// — so suspend is the verb that keeps the record resumable for the
			// next `lever up`. (`scion stop` would REMOVE the container and leave
			// a `stopped` record instead.) Gated on ResolveRunUser so a halted or
			// never-provisioned machine is still stoppable; the suspend error is
			// ignored (the VM powers off regardless, and apply's observe-first
			// start-manager copes with whatever state results). The timeout stops
			// a hung scion from blocking power-off.
			if appName != "" {
				if err := b.ResolveRunUser(cmd.Context()); err == nil {
					sctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
					sc := scion.New(b.JailRunner(), scion.Options{HubEndpoint: "http://127.0.0.1:8080"})
					_ = sc.Suspend(sctx, appName, b.MountDest())
					cancel()
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
