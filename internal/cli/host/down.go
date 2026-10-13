package host

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

func newDestroyCmd(factory BackendFactory) *cobra.Command {
	var machine, backendFlag *string
	cmd := &cobra.Command{
		Use:     "destroy",
		Aliases: []string{"down"},
		Short:   "Destroy the jail (delete the machine — full teardown)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// When tearing down the current instance, also stop the host broker and clear
			// staged state: a surviving broker's spent bootstrap latch would be reused by
			// the next apply, leaving the new manager unable to enrol.
			ia := loadInstanceApp()
			if *machine == "" {
				if ia.app != nil {
					st := stateFor(ia.path)
					// The remote proxy goes too, with its stamp: a later `up` would otherwise
					// reuse it, with a cached jail prefix naming the destroyed machine.
					stopHostDaemons(cmd, st)
					if serr := removeIfPresent(st.RemoteStamp()); serr != nil {
						cmd.PrintErrf("lever: warning: removing the remote proxy stamp: %v\n", serr)
					}
					clearStagedRuntimeState(ia.app)
					// The PATs were minted against the hub database inside the jail, which dies
					// with the machine; the next `up` must mint fresh ones, or the new hub rejects
					// them at readiness.
					if rerr := removePATs(st); rerr != nil {
						cmd.PrintErrf("lever: warning: removing stale PATs: %v\n", rerr)
					}
				}
			} else {
				cmd.PrintErrln("note: --machine given; the broker is not stopped and staged state is not cleared (run `lever destroy` from the instance root to do that).")
			}

			m, b, err := resolveJailBackendFor(factory, ia, *machine, *backendFlag)
			if err != nil {
				return err
			}
			if err := b.Teardown(cmd.Context()); err != nil {
				return err
			}
			cmd.Printf("jail %q destroyed\n", m)
			return nil
		},
	}
	machine, backendFlag = addJailTargetFlags(cmd)
	return cmd
}

// clearStagedRuntimeState removes the bootstrap ticket and runtime manifest
// staged in the mount, so they do not point at a torn-down broker.
func clearStagedRuntimeState(app *config.App) {
	_ = os.Remove(filepath.Join(app.Tree, ".lever", "bootstrap.json"))
	_ = os.Remove(filepath.Join(app.Tree, config.ManifestName))
	_ = os.Remove(filepath.Join(app.Tree, ".lever")) // removed only if now empty
}

// removePATs clears both hub tokens, their records, the remote web role
// record and the experiments pin record: all describe the hub database that
// dies with the machine, and a leftover token would be reused and rejected by
// the fresh hub (a leftover role record would stop the next apply granting).
// Every path is attempted and each failure reported.
func removePATs(st state.State) error {
	var errs []error
	for _, p := range []string{st.ControllerPAT(), st.ControllerPATRecord(), st.RemotePAT(), st.RemotePATRecord(), st.RemoteRole(), st.HubExperiments()} {
		if err := removeIfPresent(p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", filepath.Base(p), err))
		}
	}
	return errors.Join(errs...)
}

// removeIfPresent deletes path; a missing file is not an error.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
