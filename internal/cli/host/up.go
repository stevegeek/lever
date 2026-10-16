package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/convbackup"
	"github.com/stevegeek/lever/internal/progress"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/termsafe"
)

// phaseOrAbsent treats a failed phase probe as "absent" only when the error
// proves the manager cannot be running (scion.IsAgentAbsent); up then applies.
// Every other error propagates: apply is not fully idempotent (each run leaves
// a duplicate project-configs entry), so a transient list failure must not
// force a re-apply. If scion's wording changes, this fails safe by erroring.
func phaseOrAbsent(phase string, err error) (string, error) {
	if err == nil {
		return phase, nil
	}
	if scion.IsAgentAbsent(err) {
		return "", nil
	}
	return "", err
}

type upAction string

const (
	upApply upAction = "apply" // absent/stopped/error, or --fresh: full bring-up (apply discards a present record under --fresh)
	upKeep  upAction = "keep"  // running or suspended: the hub survived; the manager's lifecycle (apply.EnsureManager) keeps or resumes it
)

func upDecision(phase string, fresh bool) upAction {
	// --fresh discards any present record, and the discard is apply's job, never
	// decided on the probe here: after `lever stop` the hub is down, the probe
	// fails and the record is invisible. apply also keeps an error-phase record
	// whose forced resume comes up dead, so --fresh is the only clean escape for a
	// bricked record.
	if fresh {
		return upApply
	}
	switch phase {
	case scion.PhaseRunning, scion.PhaseSuspended:
		return upKeep
	default: // absent, stopped, error
		return upApply
	}
}

func newUpCmd(bf BackendFactory) *cobra.Command {
	var fresh, noAttach, detachOthers, noBackup, keepConv, resetHubIdentity bool
	var progressMode string
	c := &cobra.Command{
		Use:   "up [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Bring an application up (if needed) and attach the manager",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if err := checkFreshFlags(fresh, noBackup, keepConv); err != nil {
				return err
			}
			path, app, err := loadAppPath(args)
			if err != nil {
				return err
			}
			unlock, err := lockLifecycle(app, stateFor(path), "lever up")
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
			// buildApplyDeps brings the jail up through EnsureUp on every path, so on Lima
			// the guest hardening converges whether up applies, resumes or attaches.
			rep.Group("jail", 0)
			w, err := buildApplyDeps(ctx, app, path, bf, applyOpts{Cmd: cmd, Rep: rep, ResetHubIdentity: resetHubIdentity, Locked: true})
			if err != nil {
				return err
			}
			deps, b, sc := w.deps, w.b, w.sc
			project := b.MountDest() // in-jail project path == mount root

			rep.Group("manager", 0)
			check := rep.Start("check manager")
			phase, probeErr := managerPhase(ctx, sc, project, app.Name)
			phase, err = phaseOrAbsent(phase, probeErr)
			if err != nil {
				return err // possibly-transient probe failure: do NOT force apply
			}
			if probeErr != nil {
				// The probe error proves the manager is not up now (hub down, or project never
				// registered): fall through to apply. It does not prove the record is absent:
				// after `stop` a suspended record waits behind the down hub, which is why
				// --fresh rides into apply.
				check.Note(upProbeReason(probeErr))
				if fresh {
					rep.Log(upProbeNotice(probeErr, fresh))
				}
			}
			decision := upDecision(phase, fresh)
			// On a terminal the operator attaches to a manager apply created as soon as
			// claude runs, and the manager is checked after the detach. A resume waits for
			// claude's session start first, so a --continue with no conversation is
			// caught before the attach. --no-attach and a non-terminal stdin wait on every
			// path.
			attachTTY := !noAttach && isTerminal(os.Stdin)
			if probeErr == nil {
				check.Note(upDecisionNote(phase, decision))
			}
			check.Done(nil)
			// --fresh: apply's start-manager deletes any record once the hub is up and
			// creates anew; a failed delete is fatal, so the old session is never resumed
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
			switch decision {
			case upApply:
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
			case upKeep:
				// The hub survived: no plan, but the broker the manager and its
				// workers need, and the manager's lifecycle, which keeps a running
				// manager (healing its session), resumes a suspended one or one whose
				// container died under it, and gates it.
				// The role guard runs before a ticket is staged for a manager it would
				// refuse.
				if err := deps.VerifyAgentRole(ctx, hubProjectKey(project), app.Name); err != nil {
					return fmt.Errorf("start-manager: %w", err)
				}
				staged, err := ensureBrokerForUp(ctx, rep, deps, w.stepTail(apply.KindBrokerUp), app.Tree)
				if err != nil {
					return err
				}
				step := rep.Start(string(apply.KindStartManager))
				step.TailFrom(w.stepTail(apply.KindStartManager))
				_, err = apply.EnsureManager(ctx, app, deps, apply.ManagerRequest{
					Intent: apply.ManagerKeep, Subject: string(apply.KindStartManager),
					Attach: attachTTY, TicketStaged: staged, NoCreate: true,
				})
				step.Done(err)
				if err != nil {
					if saved != nil {
						rep.Log(fmt.Sprintf("lever: the previous conversation is backed up as %s; once the manager is up, `lever conversation restore %s` brings it back", saved.ID, saved.ID))
					}
					return err
				}
			}
			var remoteWarn string
			if decision != upApply {
				remoteWarn = ensureRemoteForUp(ctx, rep, app, w.remote.EnsureForUp)
			}
			stop()
			// The list ends before the attach: from here the terminal is the manager's.
			run.finish(nil)
			if remoteWarn != "" {
				logLine(cmd.ErrOrStderr(), "%s", remoteWarn)
			}
			// An attach can last all day; a new conversation must not wait on it.
			release()
			if noAttach {
				cmd.Printf("application %q is up.\n", app.Name)
				return nil
			}
			if !attachTTY {
				return runAttach(cmd.Context(), cmd.ErrOrStderr(), b, sc, app.Name, project, detachOthers)
			}
			attachErr := runAttach(cmd.Context(), cmd.ErrOrStderr(), b, sc, app.Name, project, detachOthers)
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

func upDecisionNote(phase string, d upAction) string {
	if d == upKeep {
		if phase == scion.PhaseRunning {
			return "running"
		}
		return "suspended; resuming it"
	}
	if phase == "" {
		return "no manager yet; starting it"
	}
	if phase == scion.PhaseRunning || phase == scion.PhaseSuspended {
		return fmt.Sprintf("phase %s; recreating it (--fresh)", scion.PhaseLabel(phase))
	}
	return fmt.Sprintf("phase %s; bringing it up", scion.PhaseLabel(phase))
}

// upProbeReason is the probe failure in the operator's words: a hub that is
// down answers "connection refused".
func upProbeReason(probeErr error) string {
	msg := strings.ToLower(probeErr.Error())
	switch {
	case hubNotRunning(probeErr):
		return "the jail's hub is not running (after `lever stop` or a VM restart); starting it"
	case strings.Contains(msg, "project not found"), strings.Contains(msg, "no git origin remote found"):
		return "the project is not registered with the hub yet; registering it"
	}
	return "no running manager (" + firstLine(probeErr.Error()) + "); bringing the application up"
}

// hubNotRunning reports scion's answer when the jail's hub does not serve.
func hubNotRunning(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "is not responding")
}

// upProbeNotice keeps only the first line of scion's error, which on a fresh
// machine includes the whole usage dump. Under --fresh it says what the flag
// will do, since a failed probe is when a silent resume would otherwise
// happen.
func upProbeNotice(probeErr error, fresh bool) string {
	reason := upProbeReason(probeErr)
	if fresh {
		return fmt.Sprintf("lever: no running manager observed (%s) — --fresh will discard any manager record found once the hub is up", reason)
	}
	return "lever: " + reason
}

// firstLine returns s's first line, trimmed and sanitized: scion's errors can
// carry a usage dump and jail-controlled escape sequences.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return termsafe.Sanitize(strings.TrimSpace(s))
}

func managerPhase(ctx context.Context, sc *scion.Client, project, name string) (string, error) {
	agents, err := sc.List(ctx, project)
	if err != nil {
		return "", err
	}
	if a := scion.FindAgent(agents, name); a != nil {
		return a.Phase, nil
	}
	return "", nil
}

// ensureBrokerForUp is apply's broker-up for the up path that keeps the
// manager: a broker that died under a live hub, or one of an older binary or
// config, would otherwise stay so until the next apply, and the manager and
// its workers would have no tools. staged: apply.EnsureBrokerForUp.
func ensureBrokerForUp(ctx context.Context, rep *progress.Reporter, deps apply.Deps, tail func() (string, []string), tree string) (staged bool, err error) {
	step := rep.Start(string(apply.KindBrokerUp))
	step.TailFrom(tail)
	staged, err = apply.EnsureBrokerForUp(ctx, deps, tree)
	step.Done(err)
	if err != nil {
		return false, fmt.Errorf("broker-up: %w", err)
	}
	return staged, nil
}

// ensureRemoteForUp: a proxy that is not serving does not fail the up, since
// the manager is up; the returned warning, printed once the list has ended,
// says how to bring the proxy back.
func ensureRemoteForUp(ctx context.Context, rep *progress.Reporter, app *config.App, ensure func(context.Context) error) string {
	if !app.RemoteEnabled() {
		return ""
	}
	rep.Group("remote", 0)
	step := rep.Start("remote proxy")
	err := ensure(ctx)
	switch {
	case err == nil:
		step.Done(nil)
		return ""
	case errors.Is(err, errRemoteProxyNeedsApply):
		step.Skip("needs `lever apply`")
		return "lever: remote access: " + err.Error() + ". Run `lever apply` to start the proxy with the current config."
	default:
		step.Done(err)
		return "lever: the remote-access proxy did not start: " + err.Error() + ". Run `lever apply` once the cause is fixed; `lever doctor` checks the proxy."
	}
}
