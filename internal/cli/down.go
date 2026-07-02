package cli

import (
	"os"
	"path/filepath"

	"github.com/lever-to/lever/internal/brokerctl"
	"github.com/lever-to/lever/internal/config"
	"github.com/spf13/cobra"
)

func newDownCmd(factory BackendFactory) *cobra.Command {
	var machine, backendFlag string
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Tear down the jail",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// When tearing down the current instance (no explicit --machine), also
			// stop the host-side broker and clear staged runtime state. Otherwise the
			// broker outlives the jail; its single-use bootstrap latch (already
			// consumed) then gets reused by the next `lever apply`, which stages no
			// bootstrap ticket and leaves the new manager unable to enrol.
			if machine == "" {
				if path, perr := resolveConfigPath(""); perr == nil {
					if app, lerr := config.Load(path); lerr == nil {
						if serr := brokerctl.StateDir(filepath.Dir(path)).StopBroker(); serr != nil {
							cmd.PrintErrf("warning: stopping broker: %v\n", serr)
						}
						clearStagedRuntimeState(app)
					}
				}
			} else {
				cmd.PrintErrln("note: --machine given; the broker is not stopped and staged state is not cleared (run `lever down` from the instance root to do that).")
			}

			m, err := machineFromFlagOrConfig(machine)
			if err != nil {
				return err
			}
			b, err := factory(backendFromFlagOrConfig(backendFlag), m)
			if err != nil {
				return err
			}
			if err := b.Teardown(cmd.Context()); err != nil {
				return err
			}
			cmd.Printf("jail %q down\n", m)
			return nil
		},
	}
	cmd.Flags().StringVar(&machine, "machine", "", "jail machine name (default: lever-<name> from config)")
	cmd.Flags().StringVar(&backendFlag, "backend", "", "containment backend (default: config's backend, else the registry default)")
	return cmd
}

// clearStagedRuntimeState removes the broker-dependent files the host stages into
// the mount: the one-time bootstrap ticket and the sanitized runtime manifest,
// so they don't linger pointing at a torn-down broker. Missing files are ignored.
func clearStagedRuntimeState(app *config.App) {
	_ = os.Remove(filepath.Join(app.Tree, ".lever", "bootstrap.json"))
	_ = os.Remove(filepath.Join(app.Tree, config.ManifestName))
	_ = os.Remove(filepath.Join(app.Tree, ".lever")) // removed only if now empty
}
