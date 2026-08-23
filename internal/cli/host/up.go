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
	upRestart upAction = "restart" // --fresh over a present record: delete, then apply
	upApply   upAction = "apply"   // absent/stopped/error: full bring-up
	upResume  upAction = "resume"  // suspended: resume the existing manager
	upNone    upAction = "none"    // already running: nothing to do, just attach
)

// upDecision maps the manager's current scion phase (""=absent) + --fresh to an action.
func upDecision(phase string, fresh bool) upAction {
	// --fresh discards ANY present record, whatever its phase. Since 0.12
	// apply PRESERVES an error-phase record when its forced resume comes up
	// dead (loud failure, no delete — see start-manager's #3 recovery), so
	// --fresh is the only clean escape hatch for a genuinely-bricked record;
	// limiting it to running/suspended would leave `up --fresh` resuming the
	// very record the user asked to discard.
	if fresh && phase != "" {
		return upRestart
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
			w, err := buildApplyDeps(cmd.Context(), app, path, bf, cmd)
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
				// The probe error proves the manager isn't up (hub down = fresh
				// machine; project 404 = never hub-registered) — fall through to
				// apply, which starts the hub / registers the manager, rather
				// than dying. probeErr is scion's raw CLI error, which on a
				// fresh machine includes scion's entire usage dump after the
				// first line — keep only that first line so a normal fresh
				// bring-up doesn't print a scary wall of text.
				cmd.Printf("No running manager (%s) — bringing the application up.\n", firstLine(probeErr.Error()))
			}
			switch upDecision(phase, fresh) {
			case upRestart:
				// A failed delete must be VISIBLE: with the record still
				// present, the following apply's observe-first start-manager
				// would RESUME the old conversation — silently defeating
				// --fresh (re-review residual on finding I2).
				if err := restartManagerFresh(cmd.Context(), sc, app.Name, project); err != nil {
					return fmt.Errorf("--fresh: deleting the existing manager record: %w (without this the old session would be resumed)", err)
				}
				if err := apply.Run(cmd.Context(), app, deps, apply.PlanOpts{}); err != nil {
					return err
				}
			case upApply:
				if err := apply.Run(cmd.Context(), app, deps, apply.PlanOpts{}); err != nil {
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

// firstLine returns the first line of s, trimmed of surrounding whitespace.
// Used to keep scion's raw CLI errors — which can carry an entire usage dump
// after the first line — down to one short, printable reason.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// restartManagerFresh discards the existing manager record entirely (`scion
// delete`) for the "restart" (`--fresh` over a running/suspended manager)
// decision, so the following apply's observe-first start-manager step
// (internal/apply/run.go) sees the record ABSENT and takes the CREATE path.
// It must NOT be `scion stop`: stop leaves a stopped record behind, and
// start-manager treats a stopped record as resumable — it would RESUME the
// old conversation with `claude --continue`, defeating the entire point of
// `--fresh`.
func restartManagerFresh(ctx context.Context, sc *scion.Client, name, project string) error {
	return sc.Delete(ctx, name, project)
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
