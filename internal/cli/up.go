package cli

import (
	"context"
	"strings"

	"github.com/lever-to/lever/internal/apply"
	"github.com/lever-to/lever/internal/config"
	"github.com/lever-to/lever/internal/scion"
	"github.com/spf13/cobra"
)

// phaseOrAbsent treats a failed phase probe as "absent" (no manager found)
// ONLY when the error proves the manager cannot be running (see
// managerDefinitelyAbsent). That case must fall through to upDecision
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
	if managerDefinitelyAbsent(err) {
		return "", nil
	}
	return "", err
}

// managerDefinitelyAbsent reports whether a `scion list` probe error proves
// the manager isn't up (case-insensitive match), as opposed to a transient
// failure that must propagate. Two signatures:
//
//   - hub unreachable ("is not responding" / "connection refused"): the fresh
//     machine — the hub is only started by apply's scion-server step, so
//     before the first apply nothing can be running;
//   - hub-side "project not found" (404): the hub is up but the manager
//     project was never hub-registered (e.g. a partial prior bring-up where
//     local `scion init` ran but `scion hub link` didn't) — no manager can be
//     running under a project the hub doesn't know, and apply's
//     register-manager step (init + hub link) is exactly the repair.
func managerDefinitelyAbsent(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "is not responding") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "project not found")
}

// upDecision maps the manager's current scion phase (""=absent) + --fresh to an action.
func upDecision(phase string, fresh bool) string {
	if fresh && (phase == "running" || phase == "suspended") {
		return "restart"
	}
	switch phase {
	case "running":
		return "none"
	case "suspended":
		return "resume"
	default: // absent, stopped, error
		return "apply"
	}
}

func newUpCmd(bf BackendFactory) *cobra.Command {
	var fresh, noAttach bool
	c := &cobra.Command{
		Use:   "up [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Bring an application up (if needed) and attach the manager",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath(argOrEmpty(args))
			if err != nil {
				return err
			}
			app, err := config.Load(path)
			if err != nil {
				return err
			}
			deps, b, sc, err := buildApplyDeps(cmd.Context(), app, path, bf)
			if err != nil {
				return err
			}
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
				// than dying.
				cmd.Printf("manager phase probe failed (%v) — treating as not up, applying\n", probeErr)
			}
			switch upDecision(phase, fresh) {
			case "restart":
				_ = sc.Stop(cmd.Context(), app.Name, project)
				if err := apply.Run(cmd.Context(), app, deps); err != nil {
					return err
				}
			case "apply":
				if err := apply.Run(cmd.Context(), app, deps); err != nil {
					return err
				}
			case "resume":
				if err := sc.Resume(cmd.Context(), app.Name, project); err != nil {
					return err
				}
			case "none":
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

func managerPhase(ctx context.Context, sc *scion.Client, project, name string) (string, error) {
	agents, err := sc.List(ctx, project)
	if err != nil {
		return "", err
	}
	for _, a := range agents {
		if a.Slug == name {
			return a.Phase, nil
		}
	}
	return "", nil
}
