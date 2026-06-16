package cli

import (
	"context"
	"fmt"

	"github.com/lever-to/lever/internal/apply"
	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/backend/orbstack"
	"github.com/lever-to/lever/internal/config"
	"github.com/lever-to/lever/internal/exec"
	"github.com/lever-to/lever/internal/jail"
	"github.com/lever-to/lever/internal/scion"
	"github.com/spf13/cobra"
)

func newApplyCmd(bf BackendFactory) *cobra.Command {
	var dryRun bool
	c := &cobra.Command{
		Use:   "apply CONFIG",
		Args:  cobra.ExactArgs(1),
		Short: "Bring an agent-manager application up from a config",
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := config.Load(args[0])
			if err != nil {
				return err
			}
			if dryRun {
				for _, s := range apply.Plan(app) {
					cmd.Printf("  %-16s %s\n", s.Kind, s.Target)
				}
				return nil
			}
			deps, _, _, err := buildApplyDeps(cmd.Context(), app, bf)
			if err != nil {
				return err
			}
			if err := apply.Run(cmd.Context(), app, deps); err != nil {
				return err
			}
			cmd.Printf("application %q is up.\n", app.Name)
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the bring-up plan and exit")
	return c
}

// buildApplyDeps wires the live dependencies for apply.Run.
// It eagerly calls EnsureUp so the OrbStack backend resolves the in-machine
// run-user and UID before the JailRunner and scion.Client are constructed.
// JailUp is therefore a no-op in the returned Deps — the jail is already
// confirmed up and the user/uid are known.
func buildApplyDeps(ctx context.Context, app *config.App, bf BackendFactory) (apply.Deps, *orbstack.OrbStack, *scion.Client, error) {
	machine := "lever-" + app.Name
	b := bf(machine)
	ob, ok := b.(*orbstack.OrbStack)
	if !ok {
		return apply.Deps{}, nil, nil, fmt.Errorf("apply currently supports the orbstack backend only")
	}
	cfg := backend.Config{
		MachineName:  machine,
		ProjectTree:  app.Tree,
		AllowedPorts: app.Manager.AllowPorts,
		ScionSource:  app.Scion.Source,
	}
	// Bring the jail up now so we can resolve the run-user/uid for the JailRunner.
	if err := ob.EnsureUp(ctx, cfg); err != nil {
		return apply.Deps{}, nil, nil, err
	}
	user, uid := ob.RunUser(), ob.RunUID()
	jr := jail.New(exec.RealRunner{}, machine, user, uid)
	sc := scion.New(jr, scion.Options{HubEndpoint: "http://127.0.0.1:8080"})
	return apply.Deps{
		// JailUp is a no-op: buildApplyDeps already brought the jail up
		// (idempotent; resolves user/uid). The apply executor's jail-up step
		// is thus a confirmed no-op here.
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(ctx context.Context, ref string) error { return jail.LoadImage(ctx, machine, user, uid, ref) },
		Scion:     sc,
		JailMount: ob.MountDest(),
	}, ob, sc, nil
}
