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
	upApply  upAction = "apply"  // absent/stopped/error, or --fresh: full bring-up (apply discards a present record under --fresh)
	upResume upAction = "resume" // suspended: resume the existing manager
	upNone   upAction = "none"   // already running: nothing to do, just attach
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
	case scion.PhaseRunning:
		return upNone
	case scion.PhaseSuspended:
		return upResume
	default: // absent, stopped, error
		return upApply
	}
}

// verifyManagerRole runs apply's pre-role record guard
// (apply.Deps.VerifyAgentRole) on the up paths that keep a manager without
// calling apply.Run. The hub knows the project by project's basename.
func verifyManagerRole(ctx context.Context, deps apply.Deps, project, name string) error {
	return deps.VerifyAgentRole(ctx, hubProjectKey(project), name)
}

// gateAfterUp is the liveness gate before up prints "is up." or attaches. The
// apply paths are already gated inside apply.Run. A resume gets the full gate:
// live, then held for the settle window. A manager already running gets one
// observation, no settle, and is refused only on positive evidence of a death:
// holding a healthy manager for ten seconds on every up is a poor trade.
//
// prog (may be nil) is the resume's progress line. A resume's gate waits for
// claude to report its session start whether or not a terminal attaches next,
// so a claude --continue with no conversation is named rather than attached
// to. guard (resume path only) reads the tree mounts back once the manager is
// live if that could not be done right after the resume; the manager is not
// reported up before.
func gateAfterUp(ctx context.Context, deps apply.Deps, d upAction, project, name string, prog *apply.Progress, guard *apply.ManagerResume) error {
	switch d {
	case upResume:
		err := apply.WaitManagerLive(ctx, deps, name, project, prog)
		if err != nil {
			if guard != nil {
				err = guard.AfterFailure(ctx, err)
			}
			return err
		}
		if guard == nil {
			return errors.New("up: internal error: the resume's mount checks are missing")
		}
		return guard.Live(ctx)
	case upNone:
		return apply.ObserveManagerLive(ctx, deps, name, project)
	default:
		return nil
	}
}

// resumeForUp is up's own resume, between the checks apply's start-manager
// step runs around its resume: before it the host tree and gone mount
// directories, right after it where each tree mount comes from (a mismatch
// suspends the manager). The guard's Live runs after the gate.
func resumeForUp(ctx context.Context, deps apply.Deps, app *config.App, project string, resume func(context.Context) error) (*apply.ManagerResume, error) {
	guard, err := apply.BeginManagerResume(ctx, deps, app, project, "up")
	if err != nil {
		return nil, err
	}
	if err := resume(ctx); err != nil {
		if scion.IsRefusedByHub(err) {
			err = apply.HubRefusedResume("resume", err)
		} else {
			err = apply.ResumeFailed("resume", err)
		}
		// The container may have started all the same.
		return nil, guard.AfterFailure(ctx, err)
	}
	if err := guard.Resumed(ctx); err != nil {
		return nil, err
	}
	return guard, nil
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
			w, err := buildApplyDeps(ctx, app, path, bf, applyOpts{Cmd: cmd, Rep: rep, ResetHubIdentity: resetHubIdentity})
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
			// A record left "running" by a hard VM kill (new guest boot, no live
			// container) is suspended here and resumed below. Never --fresh: the
			// conversation is kept.
			if decision == upNone && apply.RecoverAfterGuestRestart(ctx, deps, app.Name, project) {
				decision = upResume
			}
			if probeErr == nil {
				check.Note(upDecisionNote(phase, decision))
			}
			check.Done(nil)
			var prog *apply.Progress
			// guard checks the tree before the resume and reads the mounts back after it.
			var guard *apply.ManagerResume
			switch decision {
			case upApply:
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
			case upResume:
				// up resumes a suspended manager without apply.Run, so apply's pre-role guard
				// must run here too.
				if err := verifyManagerRole(ctx, deps, project, app.Name); err != nil {
					return err
				}
				guard, err = resumeForUp(ctx, deps, app, project, func(ctx context.Context) error {
					resume := rep.Start("resume manager")
					resume.TailFrom(w.stepTail(apply.KindStartManager))
					// The resume and its gate can run over a minute: print progress about every
					// 15 s, or hand the stages to the progress list.
					prog = apply.StartProgress(ctx, deps, "up", app.Name, "asking scion to resume the container")
					return sc.Resume(ctx, app.Name, project)
				})
				defer prog.Stop()
				if err != nil {
					return err
				}
			case upNone:
				// A running manager is not exempt: scion re-derives scopes from the stored
				// role on every token refresh, so an unrolled record gains full authority
				// without restarting.
				if err := verifyManagerRole(ctx, deps, project, app.Name); err != nil {
					return err
				}
				// A running manager or its workers can hold an expired hub token (the refresh
				// timer stalls while the host sleeps); this path bypasses apply.Run's heal.
				heal := rep.Start("refresh agent sessions")
				apply.HealSessions(ctx, deps, app, project)
				heal.Done(nil)
				rep.Start("check manager is live")
			}
			// Gate every path before claiming up: a harness can die a moment after its
			// first live look.
			err = gateAfterUp(ctx, deps, decision, project, app.Name, prog, guard)
			prog.Stop()
			if err != nil {
				return err
			}
			apply.RecordGuestBoot(ctx, deps)
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
				return execAttach(cmd.Context(), cmd.ErrOrStderr(), b, sc, app.Name, project, detachOthers)
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
	switch d {
	case upNone:
		return "running"
	case upResume:
		if phase == scion.PhaseRunning {
			return "the VM restarted under it; resuming it"
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
