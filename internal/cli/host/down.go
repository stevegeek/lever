package host

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hostledger"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/termsafe"
	"github.com/stevegeek/lever/internal/treebackup"
)

func newDestroyCmd(factory BackendFactory) *cobra.Command {
	var machine, backendFlag *string
	var noTreeBackup, force bool
	var lockWait time.Duration
	cmd := &cobra.Command{
		Use:     "destroy",
		Aliases: []string{"down"},
		Short:   "Destroy the jail (delete the machine — full teardown)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// When tearing down the current instance, also stop the host broker and clear
			// staged state: a surviving broker's spent bootstrap latch would be reused by
			// the next apply, leaving the new manager unable to enrol.
			ia := loadInstanceApp()
			if *machine != "" && ia.app != nil && *machine == machineName(ia.app.Name) {
				// This instance's own machine: nothing may act on it while it goes.
				// Another instance's state directory is not known here, so its machine
				// is destroyed without its lock.
				unlock, err := lockForDestroy(cmd.Context(), ia.app, stateFor(ia.path), force, lockWait, lockNotice(cmd))
				if err != nil {
					return err
				}
				defer unlock()
			}
			if *machine == "" {
				if ia.app != nil {
					st := stateFor(ia.path)
					unlock, err := lockForDestroy(cmd.Context(), ia.app, st, force, lockWait, lockNotice(cmd))
					if err != nil {
						return err
					}
					defer unlock()
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
					// It describes the machine being deleted: its guest boot and what was
					// converged in it.
					if rerr := removeIfPresent(st.LifecycleFile()); rerr != nil {
						cmd.PrintErrf("lever: warning: removing the lifecycle record: %v\n", rerr)
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
			if *machine == "" && ia.app != nil && !noTreeBackup {
				treeSnapshotAt(cmd, ia.app, stateFor(ia.path), treebackup.TriggerDestroy, quiescentAfterStop(b))
			}
			return nil
		},
	}
	machine, backendFlag = addJailTargetFlags(cmd)
	cmd.Flags().BoolVar(&noTreeBackup, "no-tree-backup", false, "do not take the tree snapshot this destroy would take (tree_backup.at_stop)")
	cmd.Flags().BoolVar(&force, "force", false, "destroy even while another lever command holds this instance's lifecycle lock (one that is stuck)")
	addLockWaitFlag(cmd, &lockWait, 0)
	return cmd
}

// lockForDestroy takes the lifecycle lock for the whole teardown, so no up,
// apply, stop, new conversation or broker heal acts on a jail being deleted.
// A held lock refuses with exit 6 and names the holder, as for the other
// lifecycle commands. destroy is the last resort, also for a holder that
// never ends (a `lever stop` stuck in the guest), so force goes on without
// the lock after a warning naming the holder.
func lockForDestroy(ctx context.Context, app *config.App, st state.State, force bool, maxWait time.Duration, warn func(string)) (func(), error) {
	unlock, err := lockLifecycle(ctx, app, st, "lever destroy", maxWait, warn)
	if err == nil {
		return unlock, nil
	}
	var ee *cli.ExitError
	if !errors.As(err, &ee) || ee.Code != exitConversationLocked {
		if !force {
			return nil, fmt.Errorf("%w. `lever destroy --force` destroys the jail without the lock", err)
		}
		warn(fmt.Sprintf("lever: warning: %v; destroying without the lifecycle lock (--force)", err))
		return func() {}, nil
	}
	holder := cmp.Or(termsafe.Sanitize(hostledger.LockHolder(st.ConversationLock())), "another lever command")
	if !force {
		return nil, cli.WithExitCode(fmt.Errorf("%s is changing this instance; nothing was destroyed. Run `lever destroy` again once it has finished. If it is stuck, end that process, or run `lever destroy --force` to destroy the jail anyway", holder), exitConversationLocked)
	}
	warn(fmt.Sprintf("lever: warning: %s holds this instance's lifecycle lock; destroying anyway (--force). That command may still act on the instance, and fail or leave state behind, once the jail is gone", holder))
	return func() {}, nil
}

// clearStagedRuntimeState removes the bootstrap ticket staged in the mount,
// so it does not point at a torn-down broker.
func clearStagedRuntimeState(app *config.App) {
	_ = os.Remove(filepath.Join(app.Tree, ".lever", "bootstrap.json"))
	_ = os.Remove(filepath.Join(app.Tree, ".lever")) // removed only if now empty
}

// removePATs clears both hub tokens, their records, the remote web role
// record, the hub identity record and the experiments pin record: all
// describe the hub database that dies with the machine, and a leftover token
// would be reused and rejected by the fresh hub. The assertion key stays: it
// is not tied to one hub database. Every path is attempted and each failure
// reported.
func removePATs(st state.State) error {
	var errs []error
	for _, p := range []string{st.ControllerPAT(), st.ControllerPATRecord(), st.RemotePAT(), st.RemotePATRecord(), st.RemoteRole(), st.HubIdentityFile(), st.HubExperiments()} {
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
