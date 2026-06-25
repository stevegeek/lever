package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/lever-to/lever/internal/apply"
	"github.com/lever-to/lever/internal/config"
	"github.com/lever-to/lever/internal/jail"
	"github.com/lever-to/lever/internal/scion"
	"github.com/spf13/cobra"
)

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
			deps, ob, sc, err := buildApplyDeps(cmd.Context(), app, path, bf)
			if err != nil {
				return err
			}
			project := ob.MountDest() // in-jail project path == mount root

			phase, err := managerPhase(cmd.Context(), sc, project, app.Name)
			if err != nil {
				return err
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
			inner := sc.AttachArgv(app.Name, project)
			argv := jail.AttachArgv(ob.MachineName(), ob.RunUser(), ob.RunUID(), inner)
			bin, err := exec.LookPath(argv[0])
			if err != nil {
				return fmt.Errorf("attach: %w", err)
			}
			return syscall.Exec(bin, argv, os.Environ()) // hand over the TTY
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
