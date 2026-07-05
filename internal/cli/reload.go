package cli

import (
	"path/filepath"

	"github.com/lever-to/lever/internal/apply"
	"github.com/lever-to/lever/internal/brokerctl"
	"github.com/lever-to/lever/internal/config"
	"github.com/spf13/cobra"
)

// newReloadCmd applies config changes to an ALREADY-RUNNING instance without a
// full VM power cycle. The broker reads lever.yaml once, at its own startup, and
// a plain re-apply deliberately keeps a healthy broker alive — so an edited
// config (new grove, tool, or grant) otherwise takes no effect until the broker
// restarts. reload forces exactly that: it stops the broker, then runs the same
// idempotent apply plan, which re-reads the config, re-registers groves,
// re-applies egress, and spawns a fresh broker. The VM stays up and apply's
// observe-first start-manager sees the manager still running (no-op), so the
// manager's conversation is preserved — this is the broker half of a
// `lever stop` + `lever up` without the power cycle or the re-attach.
func newReloadCmd(bf BackendFactory) *cobra.Command {
	c := &cobra.Command{
		Use:   "reload [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Apply config changes to a running instance (restart the broker, keep the manager)",
		// A config/bring-up failure is a diagnosis, not a usage error.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath(argOrEmpty(args))
			if err != nil {
				return err
			}
			app, err := config.Load(path)
			if err != nil {
				return err
			}
			// Stop the running broker FIRST so the apply's start-broker probe
			// finds nothing serving and spawns a fresh process on the new config
			// (its keep-existing branch would otherwise leave the stale broker,
			// and the config change, in place). Idempotent: a no-op if none runs.
			if err := brokerctl.StateDir(filepath.Dir(path)).StopBroker(); err != nil {
				return err
			}
			deps, _, _, err := buildApplyDeps(cmd.Context(), app, path, bf, cmd)
			if err != nil {
				return err
			}
			if err := apply.Run(cmd.Context(), app, deps); err != nil {
				return err
			}
			cmd.Printf("application %q reloaded (broker restarted on the current config).\n", app.Name)
			return nil
		},
	}
	return c
}
