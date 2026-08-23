package host

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/config"
)

func newDestroyCmd(factory BackendFactory) *cobra.Command {
	var machine, backendFlag *string
	cmd := &cobra.Command{
		Use:     "destroy",
		Aliases: []string{"down"},
		Short:   "Destroy the jail (delete the machine — full teardown)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// When tearing down the current instance (no explicit --machine), also
			// stop the host-side broker and clear staged runtime state. Otherwise the
			// broker outlives the jail; its single-use bootstrap latch (already
			// consumed) then gets reused by the next `lever apply`, which stages no
			// bootstrap ticket and leaves the new manager unable to enrol.
			ia := loadInstanceApp()
			if *machine == "" {
				if ia.app != nil {
					st := stateFor(ia.path)
					// The remote proxy is a host-side daemon like the broker,
					// and a verb documented as full teardown must stop it too.
					// Leaving it running kept two loopback listeners and a
					// `tailscale serve` front end pointed at a destroyed
					// instance — and worse, a later `up` REUSED that process,
					// whose cached jail prefix names the machine that no
					// longer exists. Its stamp goes with it: nothing else
					// removes it, and a stale stamp is what makes the reuse
					// look legitimate.
					stopHostDaemons(cmd, st)
					if serr := removeIfPresent(st.RemoteStamp()); serr != nil {
						cmd.PrintErrf("warning: removing the remote proxy stamp: %v\n", serr)
					}
					clearStagedRuntimeState(ia.app)
					// The controller PAT was minted against the hub DB that lives
					// inside the jail; destroying the machine discards that DB, so
					// the persisted PAT is now stale. Remove it so the next `up`
					// mints a fresh one — otherwise ensureControllerPAT's idempotent
					// no-op reuses the stale PAT and the new hub's fresh DB rejects
					// it ("authentication failed" at readiness).
					if rerr := removeIfPresent(st.ControllerPAT()); rerr != nil {
						cmd.PrintErrf("warning: removing stale controller PAT: %v\n", rerr)
					}
					// Same reasoning applies to the remote-access PAT: it is minted
					// against the same jail hub DB, so it goes stale the moment the
					// DB dies with the machine. Left behind, ensureControllerPAT's
					// needRemote check would see it as still-present and skip the
					// re-mint on the next `up`, leaving the remote proxy injecting a
					// token the fresh hub has never issued.
					if rerr := removeIfPresent(st.RemotePAT()); rerr != nil {
						cmd.PrintErrf("warning: removing stale remote PAT: %v\n", rerr)
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

// clearStagedRuntimeState removes the broker-dependent files the host stages into
// the mount: the one-time bootstrap ticket and the sanitized runtime manifest,
// so they don't linger pointing at a torn-down broker. Missing files are ignored.
func clearStagedRuntimeState(app *config.App) {
	_ = os.Remove(filepath.Join(app.Tree, ".lever", "bootstrap.json"))
	_ = os.Remove(filepath.Join(app.Tree, config.ManifestName))
	_ = os.Remove(filepath.Join(app.Tree, ".lever")) // removed only if now empty
}

// removeIfPresent deletes path. A missing file is not an error; any other
// removal failure is returned so the caller can warn.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
