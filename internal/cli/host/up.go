package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/termsafe"
)

// phaseOrAbsent treats a failed phase probe as "absent" (no manager found)
// ONLY when the error proves the manager cannot be running (see
// scion.IsAgentAbsent). That case must fall through to upDecision
// (-> "apply"), not abort `up`.
//
// Every other probe error propagates unchanged: `lever apply` is NOT fully
// idempotent (each run leaves a duplicate scion project-configs entry), so a
// transient list failure (auth blip, malformed output) on an already-up
// instance must not force a re-apply. This scoping also fails safe — if
// scion's wording ever changes, we regress to the OLD behavior (error out),
// never to a harmful forced re-apply.
func phaseOrAbsent(phase string, err error) (string, error) {
	if err == nil {
		return phase, nil
	}
	if scion.IsAgentAbsent(err) {
		return "", nil
	}
	return "", err
}

// upAction is the action `up` takes for the manager's current state, as
// decided by upDecision and dispatched in newUpCmd's switch.
type upAction string

const (
	upApply  upAction = "apply"  // absent/stopped/error, or --fresh: full bring-up (apply discards a present record under --fresh)
	upResume upAction = "resume" // suspended: resume the existing manager
	upNone   upAction = "none"   // already running: nothing to do, just attach
)

// upDecision maps the manager's current scion phase (""=absent) + --fresh to an action.
func upDecision(phase string, fresh bool) upAction {
	// --fresh discards ANY present record, whatever its phase, and the
	// discard is apply's job (PlanOpts.Fresh, start-manager's converge step),
	// never decided on the probe here: after `lever stop` the hub is down
	// until apply's scion-server step, so the probe fails and the record is
	// invisible — deciding on the probe dropped --fresh on exactly that path
	// (lever#33). Since 0.12 apply also PRESERVES an error-phase record when
	// its forced resume comes up dead (#3), so --fresh reaching apply is the
	// only clean escape hatch for a genuinely-bricked record.
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

// verifyManagerRole runs apply's pre-role record guard (see
// apply.Deps.VerifyAgentRole) on the `up` paths that keep an existing manager
// record WITHOUT calling apply.Run. project is the in-jail mount root; the hub
// knows the project by its basename, exactly as apply's own call does.
func verifyManagerRole(ctx context.Context, deps apply.Deps, project, name string) error {
	return deps.VerifyAgentRole(ctx, hubProjectKey(project), name)
}

// gateAfterUp is the liveness gate `up` runs before it prints "is up." or
// attaches, chosen by the path taken. The apply paths (create, --fresh) are
// already gated inside apply.Run's start-manager step, settle window included,
// so they add nothing here. A resume acted on the manager moments ago and
// gets the full gate: live, then held for the settle window. A manager that
// was already running when `up` looked gets one observation, no settle (a
// failed list is retried a few times) and is refused only on positive
// evidence of a death — a harness that died earlier reads as an error phase
// or an exited container — because holding a healthy long-running manager
// for ten seconds on every `up` would be a poor trade.
//
// prog (may be nil) is the resume's progress line (lever#168), handed the
// gate's stages.
func gateAfterUp(ctx context.Context, deps apply.Deps, d upAction, project, name string, prog *apply.Progress) error {
	switch d {
	case upResume:
		return apply.WaitManagerLive(ctx, deps, name, project, prog)
	case upNone:
		return apply.ObserveManagerLive(ctx, deps, name, project)
	default:
		return nil
	}
}

func newUpCmd(bf BackendFactory) *cobra.Command {
	var fresh, noAttach bool
	var progressMode string
	c := &cobra.Command{
		Use:   "up [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Bring an application up (if needed) and attach the manager",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			path, app, err := loadAppPath(args)
			if err != nil {
				return err
			}
			// The first line of every up, before the jail is touched: what
			// runs, on which backend (lever#195). A VM boot can take a
			// minute; it is never silent.
			run, pctx, err := startBringup(cmd, app, stateFor(path), "up", progressMode)
			if err != nil {
				return err
			}
			defer func() { run.finish(err) }()
			rep := run.rep
			printRemoteWarnings(cmd, app)
			if err := checkContactGate(app, stateFor(path)); err != nil {
				return err
			}
			printContactSessionWarnings(cmd, app, stateFor(path))
			// The bring-up runs under a signal-aware context (see
			// applySignalContext); the attach below does not, so the
			// operator's keys go to the manager, not to this handler.
			ctx, stop := applySignalContext(pctx)
			defer stop()
			// buildApplyDeps brings the jail up through the backend's EnsureUp
			// on every path below, before up looks at the manager: on Lima
			// that converges the guest hardening (crash recovery, guest_kernel;
			// lever#179) whether up then applies, resumes or only attaches.
			rep.Group("jail", 0)
			w, err := buildApplyDeps(ctx, app, path, bf, applyOpts{Cmd: cmd, Rep: rep})
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
				// The probe error proves the manager isn't up NOW (hub down
				// after `lever stop` or on a fresh machine; project 404 =
				// never hub-registered) — fall through to apply, which starts
				// the hub / registers the manager, rather than dying. It does
				// NOT prove the record is absent: after `stop` a suspended
				// record is waiting behind the down hub, which is why --fresh
				// rides into apply rather than being decided here (lever#33).
				check.Note(upProbeReason(probeErr))
				if fresh {
					rep.Log(upProbeNotice(probeErr, fresh))
				}
			}
			decision := upDecision(phase, fresh)
			// A record left "running" by a hard VM kill (a new guest boot,
			// no live container) is suspended here and resumed below: what
			// the operator used to do by hand with `lever stop` + `lever up`
			// (lever#179). Never --fresh: the conversation is kept.
			if decision == upNone && apply.RecoverAfterGuestRestart(ctx, deps, app.Name, project) {
				decision = upResume
			}
			if probeErr == nil {
				check.Note(upDecisionNote(phase, decision))
			}
			check.Done(nil)
			var prog *apply.Progress
			switch decision {
			case upApply:
				// --fresh: apply's start-manager step deletes any record it
				// finds once the hub is up and creates anew; a failed delete
				// is fatal there, so the old session is never resumed silently.
				if err := apply.Run(ctx, app, deps, apply.PlanOpts{Fresh: fresh}); err != nil {
					return err
				}
			case upResume:
				// `up` resumes a suspended manager ITSELF, without going through
				// apply.Run, so apply's pre-role record guard would not run on
				// the commonest path of all (stop suspends; up resumes). Run it
				// here for the same reason apply does.
				if err := verifyManagerRole(ctx, deps, project, app.Name); err != nil {
					return err
				}
				// shared_folders: the resume re-resolves every mount source,
				// as apply's start-manager step checks before its resume.
				if err := app.PrepareSharedFoldersHost(); err != nil {
					return err
				}
				resume := rep.Start("resume manager")
				resume.TailFrom(w.stepTail(apply.KindStartManager))
				// The resume and the gate after it can run over a minute
				// (container start, pre-start hook, claude, settle): print
				// progress about every 15 s (lever#168), or hand the stages
				// to the progress list. Stopped on every return; the gate
				// below gets the stages.
				prog = apply.StartProgress(ctx, deps, "up", app.Name, "asking scion to resume the container")
				defer prog.Stop()
				if err := sc.Resume(ctx, app.Name, project); err != nil {
					if scion.IsRefusedByHub(err) {
						return apply.HubRefusedResume("resume", err)
					}
					return apply.ResumeFailed("resume", err)
				}
			case upNone:
				// A running manager is not exempt: it refreshes its own token
				// periodically, and scion#1101 re-derives scopes from the stored
				// role on every refresh, so an unrolled record acquires full
				// authority without ever restarting. `lever attach` still
				// reaches the manager if the operator needs it while deciding.
				if err := verifyManagerRole(ctx, deps, project, app.Name); err != nil {
					return err
				}
				// A running manager can still hold an expired hub token (the
				// refresh timer stalls while the host sleeps), and its workers
				// too; apply.Run would heal them, but this path bypasses it.
				heal := rep.Start("refresh agent sessions")
				apply.HealSessions(ctx, deps, app, project)
				heal.Done(nil)
				rep.Start("check manager is live")
			}
			// The paths that act on the manager without apply.Run used to reach
			// the "is up." print with no observation at all, and the apply
			// paths returned on the first live look — over a harness that dies
			// a moment later (lever#31). Gate every path before claiming up.
			err = gateAfterUp(ctx, deps, decision, project, app.Name, prog)
			prog.Stop()
			if err != nil {
				return err
			}
			apply.RecordGuestBoot(ctx, deps)
			stop()
			// The list ends before the attach: from here the terminal is the
			// manager's.
			run.finish(nil)
			run = nil
			if noAttach {
				cmd.Printf("application %q is up.\n", app.Name)
				return nil
			}
			return execAttach(cmd.Context(), b, sc, app.Name, project)
		},
	}
	c.Flags().BoolVar(&fresh, "fresh", false, "start a fresh manager thread")
	c.Flags().BoolVar(&noAttach, "no-attach", false, "bring up but do not attach")
	c.Flags().StringVar(&progressMode, "progress", "", progressFlagUsage)
	return c
}

// upDecisionNote says, on the check step, what up found and will do.
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
	return fmt.Sprintf("phase %s; bringing it up", scion.PhaseLabel(phase))
}

// upProbeReason is the probe failure in the operator's words (lever#195):
// the hub after `lever stop` or a VM restart answers "connection refused",
// which says nothing to someone who did not write the hub client.
func upProbeReason(probeErr error) string {
	msg := strings.ToLower(probeErr.Error())
	switch {
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "is not responding"):
		return "the jail's hub is not running (after `lever stop` or a VM restart); starting it"
	case strings.Contains(msg, "project not found"), strings.Contains(msg, "no git origin remote found"):
		return "the project is not registered with the hub yet; registering it"
	}
	return "no running manager (" + firstLine(probeErr.Error()) + "); bringing the application up"
}

// upProbeNotice is the line `up` prints when the phase probe could not see
// the manager. probeErr is scion's raw CLI error, which on a fresh machine
// includes scion's entire usage dump after the first line — keep only that
// first line so a normal bring-up doesn't print a scary wall of text. Under
// --fresh it must say what the flag will do, because the probe failing is
// exactly the case where a silent resume used to happen (lever#33).
func upProbeNotice(probeErr error, fresh bool) string {
	reason := upProbeReason(probeErr)
	if fresh {
		return fmt.Sprintf("lever: no running manager observed (%s) — --fresh will discard any manager record found once the hub is up", reason)
	}
	return "lever: " + reason
}

// firstLine returns the first line of s, trimmed of surrounding whitespace
// and sanitized for the terminal. Used to keep scion's raw CLI errors —
// which can carry an entire usage dump after the first line, and which the
// jail can lace with escape sequences — down to one short, printable reason.
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
