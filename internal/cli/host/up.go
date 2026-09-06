package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/scion"
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
func gateAfterUp(ctx context.Context, deps apply.Deps, d upAction, project, name string) error {
	switch d {
	case upResume:
		return apply.WaitManagerLive(ctx, deps, name, project)
	case upNone:
		return apply.ObserveManagerLive(ctx, deps, name, project)
	default:
		return nil
	}
}

func newUpCmd(bf BackendFactory) *cobra.Command {
	var fresh, noAttach bool
	c := &cobra.Command{
		Use:   "up [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Bring an application up (if needed) and attach the manager",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, app, err := loadAppPath(args)
			if err != nil {
				return err
			}
			w, err := buildApplyDeps(cmd.Context(), app, path, bf, applyOpts{Cmd: cmd})
			if err != nil {
				return err
			}
			deps, b, sc := w.deps, w.b, w.sc
			project := b.MountDest() // in-jail project path == mount root

			phase, probeErr := managerPhase(cmd.Context(), sc, project, app.Name)
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
				cmd.Println(upProbeNotice(probeErr, fresh))
			}
			decision := upDecision(phase, fresh)
			switch decision {
			case upApply:
				// --fresh: apply's start-manager step deletes any record it
				// finds once the hub is up and creates anew; a failed delete
				// is fatal there, so the old session is never resumed silently.
				if err := apply.Run(cmd.Context(), app, deps, apply.PlanOpts{Fresh: fresh}); err != nil {
					return err
				}
			case upResume:
				// `up` resumes a suspended manager ITSELF, without going through
				// apply.Run, so apply's pre-role record guard would not run on
				// the commonest path of all (stop suspends; up resumes). Run it
				// here for the same reason apply does.
				if err := verifyManagerRole(cmd.Context(), deps, project, app.Name); err != nil {
					return err
				}
				if err := sc.Resume(cmd.Context(), app.Name, project); err != nil {
					return err
				}
			case upNone:
				// A running manager is not exempt: it refreshes its own token
				// periodically, and scion#1101 re-derives scopes from the stored
				// role on every refresh, so an unrolled record acquires full
				// authority without ever restarting. `lever attach` still
				// reaches the manager if the operator needs it while deciding.
				if err := verifyManagerRole(cmd.Context(), deps, project, app.Name); err != nil {
					return err
				}
			}
			// The paths that act on the manager without apply.Run used to reach
			// the "is up." print with no observation at all, and the apply
			// paths returned on the first live look — over a harness that dies
			// a moment later (lever#31). Gate every path before claiming up.
			if err := gateAfterUp(cmd.Context(), deps, decision, project, app.Name); err != nil {
				return err
			}
			if noAttach {
				cmd.Printf("application %q is up.\n", app.Name)
				return nil
			}
			return execAttach(b, sc, app.Name, project)
		},
	}
	c.Flags().BoolVar(&fresh, "fresh", false, "start a fresh manager thread")
	c.Flags().BoolVar(&noAttach, "no-attach", false, "bring up but do not attach")
	return c
}

// upProbeNotice is the line `up` prints when the phase probe could not see
// the manager. probeErr is scion's raw CLI error, which on a fresh machine
// includes scion's entire usage dump after the first line — keep only that
// first line so a normal bring-up doesn't print a scary wall of text. Under
// --fresh it must say what the flag will do, because the probe failing is
// exactly the case where a silent resume used to happen (lever#33).
func upProbeNotice(probeErr error, fresh bool) string {
	reason := firstLine(probeErr.Error())
	if fresh {
		return fmt.Sprintf("No running manager observed (%s) — bringing the application up; --fresh will discard any manager record found once the hub is up.", reason)
	}
	return fmt.Sprintf("No running manager (%s) — bringing the application up.", reason)
}

// firstLine returns the first line of s, trimmed of surrounding whitespace.
// Used to keep scion's raw CLI errors — which can carry an entire usage dump
// after the first line — down to one short, printable reason.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
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
