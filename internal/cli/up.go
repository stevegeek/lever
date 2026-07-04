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
// failure that must propagate. Three signatures:
//
//   - hub unreachable ("is not responding" / "connection refused"): the fresh
//     machine — the hub is only started by apply's scion-server step, so
//     before the first apply nothing can be running;
//   - hub-side "project not found" (404): the hub is up but the manager
//     project was never hub-registered (e.g. a partial prior bring-up where
//     local `scion init` ran but `scion hub link` didn't) — no manager can be
//     running under a project the hub doesn't know, and apply's
//     register-manager step (init + hub link) is exactly the repair;
//   - "no git origin remote found": scion's documented fallback when the path
//     isn't a locally registered project at all (no ~/.scion/project-configs
//     entry — forced project resolution falls back to git; see the
//     internal/scion/bringup.go waitHubReady comment documenting this exact
//     string). Lever projects are directory projects, never git-resolved, so
//     for us this can only mean "not registered" — again definitively absent,
//     and apply's register-manager is the repair.
func managerDefinitelyAbsent(err error) bool {
	msg := strings.ToLower(err.Error())
	return hubUnreachable(err) ||
		strings.Contains(msg, "project not found") ||
		strings.Contains(msg, "no git origin remote found")
}

// hubUnreachable reports whether err proves the scion HUB PROCESS itself is
// down, as opposed to the hub being up but the manager project unregistered
// (project-not-found / no-git-origin — restarting the hub can't fix those).
// This is the subset of managerDefinitelyAbsent worth retrying after an
// idempotent hub restart: a truly fresh machine (hub never started) and a
// `lever stop` -> `up` cycle (OrbStack disk — and scion's hub.db, with the
// suspended manager still registered — persists across power-off, but the
// hub PROCESS does not survive it) look IDENTICAL from here, both surfacing
// as "hub not responding". See resolveManagerPhase.
func hubUnreachable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "is not responding") ||
		strings.Contains(msg, "connection refused")
}

// resolveManagerPhase runs probe once, and — ONLY when the machine
// PRE-EXISTED this `up` (machinePreexisted) AND the failure proves the hub
// process itself is unreachable (hubUnreachable) — restarts the hub
// (idempotent: ServerStart tolerates an already-running server) and probes
// exactly once more before classifying via phaseOrAbsent. This is the
// stop->up warm-resume path: a restart turns "hub not responding" on a real
// stop->up into a successful re-probe that surfaces the persisted, suspended
// manager (-> upDecision "resume"), instead of falling through to a full
// re-apply (which would add a duplicate scion project-config and start a
// FRESH manager thread rather than resuming the existing one).
//
// machinePreexisted MUST be false for a machine CREATED this run (backend
// Created()==true): a fresh machine's hub is ALSO unreachable (it hasn't been
// started yet), and restarting it here would start the scion server BEFORE
// apply's init-machine/config-registry steps configure it — the server then
// comes up with allow_container_script_harnesses=false, and apply's later
// start-manager step 403s. Gating on machinePreexisted keeps the fresh path
// exactly as it was before warm-resume existed: apply owns the ordered
// bring-up (init-machine -> config-registry -> scion-server -> ...).
//
// A restartHub error is ignored — restarting is a best-effort upgrade, not a
// requirement — and we fall through classifying the ORIGINAL probe result.
// Only hubUnreachable (not the wider managerDefinitelyAbsent) triggers a
// restart: "project not found" / "no git origin" mean the hub is UP but
// nothing is registered, where restarting cannot help.
func resolveManagerPhase(machinePreexisted bool, probe func() (string, error), restartHub func() error) (string, error) {
	phase, err := probe()
	if machinePreexisted && err != nil && hubUnreachable(err) {
		if restartHub() == nil {
			phase, err = probe()
		}
	}
	return phaseOrAbsent(phase, err)
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

			phase, err := resolveManagerPhase(
				!b.Created(),
				func() (string, error) { return managerPhase(cmd.Context(), sc, project, app.Name) },
				func() error { return sc.ServerStart(cmd.Context()) },
			)
			if err != nil {
				return err // possibly-transient probe failure: do NOT force apply
			}
			decision := upDecision(phase, fresh)
			switch {
			case phase == "":
				// Absent after resolveManagerPhase's classification (fresh
				// machine, never-registered project, or a stop->up whose
				// warm-resume restart-and-reprobe still found nothing) —
				// fall through to apply, which starts the hub / registers
				// the manager, rather than dying.
				cmd.Printf("No running manager — bringing the application up.\n")
			case decision == "resume":
				// The warm-resume win: a stop->up whose restart-and-reprobe
				// surfaced the persisted, suspended manager.
				cmd.Printf("Resuming the suspended manager.\n")
			}
			switch decision {
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
	for _, a := range agents {
		if a.Slug == name {
			return a.Phase, nil
		}
	}
	return "", nil
}
