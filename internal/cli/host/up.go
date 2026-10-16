package host

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/convbackup"
	"github.com/stevegeek/lever/internal/termsafe"
)

func newUpCmd(bf BackendFactory) *cobra.Command {
	var fresh, noAttach, detachOthers, noBackup, keepConv, resetHubIdentity bool
	var progressMode string
	var lockWait time.Duration
	c := &cobra.Command{
		Use:   "up [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Converge an application on its config, as apply does, and attach the manager",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if err := checkFreshFlags(fresh, noBackup, keepConv); err != nil {
				return err
			}
			path, app, err := loadAppPath(args)
			if err != nil {
				return err
			}
			unlock, err := lockLifecycle(cmd.Context(), app, stateFor(path), "lever up", lockWait, lockNotice(cmd))
			if err != nil {
				return err
			}
			release := sync.OnceFunc(unlock)
			defer release()
			// The first line of every up, before the jail is touched: what runs, on which
			// backend. A VM boot can take a minute and is never silent.
			run, pctx, err := startBringup(cmd, app, stateFor(path), "up", progressMode)
			if err != nil {
				return err
			}
			defer run.finishOnReturn(&err)
			rep := run.rep
			printRemoteWarnings(cmd, app)
			if err := fenceNow(app, stateFor(path)); err != nil {
				return err
			}
			if err := checkContactGate(app, stateFor(path)); err != nil {
				return err
			}
			printContactSessionWarnings(cmd, app, stateFor(path))
			// The bring-up runs under a signal-aware context; the attach does not, so the
			// operator's keys go to the manager.
			ctx, stop := applySignalContext(pctx)
			defer stop()
			rep.Group("jail", 0)
			w, err := buildApplyDeps(ctx, app, path, bf, applyOpts{Cmd: cmd, Rep: rep, ResetHubIdentity: resetHubIdentity, Locked: true})
			if err != nil {
				return err
			}
			deps, b, sc := w.deps, w.b, w.sc
			project := b.MountDest() // in-jail project path == mount root

			// On a terminal the operator attaches to a manager the run created as soon
			// as claude runs, and the manager is checked after the detach. A resume
			// waits for claude's session start first, so a --continue with no
			// conversation is caught before the attach. --no-attach and a
			// non-terminal stdin wait on every path.
			attachTTY := !noAttach && isTerminal(os.Stdin)
			// --fresh: start-manager deletes any record once the hub is up and creates
			// anew; a failed delete is fatal, so the old session is never resumed
			// silently. Unless --no-backup the conversation is backed up first, and a
			// failed backup keeps the record.
			//
			// Without --fresh, a resume whose claude finds no conversation is started
			// fresh once the guest shows the home holds no transcript; the backup guards
			// that start too.
			var saved *convbackup.Meta
			conv := newConvOps(w, app, stateFor(path))
			switch {
			case fresh && !noBackup:
				deps.BackupManagerConversation = conv.backupBeforeDelete(&saved)
			case !fresh:
				deps.BackupManagerConversation = conv.backupBeforeNoConversationFresh(&saved)
			}
			deps.ManagerConversationEmpty = conv.conversationEmpty
			// The whole plan, as apply runs it: each step converges on the config as
			// it is now, and skips only what its own check finds done.
			if err := apply.Run(ctx, app, deps, apply.PlanOpts{Fresh: fresh, Attach: attachTTY, FreshWithoutConversation: !fresh}); err != nil {
				if saved != nil {
					rep.Log(fmt.Sprintf("lever: the previous conversation is backed up as %s; once the manager is up, `lever conversation restore %s` brings it back", saved.ID, saved.ID))
				}
				return err
			}
			if keepConv {
				rep.Group("conversation", 0)
				step := rep.Start("restore conversation")
				if saved == nil {
					step.Skip("there was no conversation to keep")
				} else {
					err := conv.restore(ctx, saved.ID, false)
					step.Done(err)
					if err != nil {
						return fmt.Errorf("%w. The previous conversation is backed up as %s: `lever conversation restore %s` tries again", err, saved.ID, saved.ID)
					}
				}
			}
			// The attach's token is staged while the lock is held, so the attach
			// itself needs nothing of it.
			var attachArgs []string
			if !noAttach {
				if attachArgs, err = prepareAttach(cmd.Context(), cmd.ErrOrStderr(), b, sc, app.Name, project, detachOthers); err != nil {
					return err
				}
			}
			stop()
			// The list ends before the attach: from here the terminal is the manager's.
			run.finish(nil)
			// An attach can last all day; a new conversation must not wait on it.
			release()
			if noAttach {
				cmd.Printf("application %q is up.\n", app.Name)
				return nil
			}
			if !attachTTY {
				return attachChild(attachArgs, os.Stdin, os.Stdout, os.Stderr)
			}
			attachErr := attachChild(attachArgs, os.Stdin, os.Stdout, os.Stderr)
			if holder := managerLockHolder(app, stateFor(path)); holder != "" {
				// Its stop ended the attach; the manager is down by design.
				cmd.PrintErrf("lever: the attach ended because %s is changing the manager; `lever up` attaches again once it has finished.\n", holder)
				return attachErr
			}
			if err := apply.CheckManagerAfterDetach(cmd.Context(), deps, app.Name, project); err != nil {
				return err
			}
			return attachErr
		},
	}
	c.Flags().BoolVar(&fresh, "fresh", false, "start a fresh manager thread (the old conversation is backed up first)")
	c.Flags().BoolVar(&resetHubIdentity, "reset-hub-identity", false, resetHubIdentityUsage)
	c.Flags().BoolVar(&noBackup, "no-backup", false, "with --fresh: discard the old conversation without backing it up")
	c.Flags().BoolVar(&keepConv, "keep-conversation", false, "with --fresh: recreate the manager and resume it on the old conversation")
	c.Flags().BoolVar(&noAttach, "no-attach", false, "bring up but do not attach")
	c.Flags().BoolVar(&detachOthers, "detach-others", false, detachOthersFlagUsage)
	c.Flags().StringVar(&progressMode, "progress", "", progressFlagUsage)
	addLockWaitFlag(c, &lockWait, 0)
	return c
}

// checkFreshFlags: --no-backup and --keep-conversation act on the record
// --fresh deletes, and keeping a conversation needs its backup.
func checkFreshFlags(fresh, noBackup, keepConv bool) error {
	switch {
	case (noBackup || keepConv) && !fresh:
		return usageError{errors.New("--no-backup and --keep-conversation go with --fresh")}
	case noBackup && keepConv:
		return usageError{errors.New("--keep-conversation needs the backup that --no-backup skips")}
	}
	return nil
}

// hubNotRunning reports scion's answer when the jail's hub does not serve.
func hubNotRunning(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "is not responding")
}

// firstLine returns s's first line, trimmed and sanitized: scion's errors can
// carry a usage dump and jail-controlled escape sequences.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return termsafe.Sanitize(strings.TrimSpace(s))
}
