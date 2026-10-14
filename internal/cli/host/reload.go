package host

import (
	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/brokerctl"
)

// newReloadCmd applies config changes to a running instance without a VM power
// cycle. The broker reads lever.yaml only at startup and a re-apply keeps a
// healthy broker, so reload stops the broker and runs the same idempotent
// apply plan. The VM stays up and the running manager is kept, so its
// conversation is preserved.
func newReloadCmd(bf BackendFactory) *cobra.Command {
	c := &cobra.Command{
		Use:   "reload [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Apply config changes to a running instance (restart the broker, keep the manager)",
		// A config or bring-up failure is a diagnosis, not a usage error.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, app, err := loadAppPath(args)
			if err != nil {
				return err
			}
			unlock, err := lockLifecycle(app, stateFor(path), "lever reload")
			if err != nil {
				return err
			}
			defer unlock()
			// Stop the broker first, or apply's start-broker would keep the stale one and
			// the old config. A no-op if none runs.
			if err := brokerctl.StopBroker(stateFor(path)); err != nil {
				return err
			}
			ctx, stop := applySignalContext(cmd.Context())
			defer stop()
			w, err := buildApplyDeps(ctx, app, path, bf, applyOpts{Cmd: cmd})
			if err != nil {
				return err
			}
			if err := apply.Run(ctx, app, w.deps, apply.PlanOpts{}); err != nil {
				return err
			}
			cmd.Printf("application %q reloaded (broker restarted on the current config).\n", app.Name)
			return nil
		},
	}
	return c
}
