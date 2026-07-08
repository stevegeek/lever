package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/scion"
)

// attachTarget resolves NAME ("" = manager) to the scion slug + jail project to
// attach. Under the single-project model the manager and every worker are agents
// in the ONE instance project (the jail mount root), distinguished by slug — so
// both resolve to mountDest as the project; only the slug differs. Unknown names
// error with the full list of valid targets.
func attachTarget(app *config.App, mountDest, name string) (slug, project string, err error) {
	if name == "" || name == app.Name {
		return app.Name, mountDest, nil
	}
	names := []string{app.Name}
	for _, g := range app.Workers {
		if g.Name == name {
			return g.Name, mountDest, nil
		}
		names = append(names, g.Name)
	}
	return "", "", fmt.Errorf("attach: unknown agent %q (valid: %s)", name, strings.Join(names, ", "))
}

// execAttach replaces the current process with the backend-wrapped scion attach
// for slug in project — the same TTY-handover chain `lever up` uses. It only
// returns on error (syscall.Exec does not return on success).
func execAttach(b backend.Backend, sc *scion.Client, slug, project string) error {
	argv := b.AttachArgv(sc.AttachArgv(slug, project))
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	return syscall.Exec(bin, argv, os.Environ()) // hand over the TTY
}

// newAttachCmd is a debugging/eyes-on verb: it attaches to a RUNNING agent and
// deliberately does no lifecycle work (bring things up with `lever up`). It is
// strictly passive: if the jail itself is not up, ResolveRunUser fails fast
// rather than provisioning it. If the jail is up but the target agent/worker is
// not running, scion's own attach error surfaces.
func newAttachCmd(bf BackendFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "attach [NAME]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Attach your TTY to the manager (default) or a named worker agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath, err := resolveConfigPath("")
			if err != nil {
				return err
			}
			app, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			b, err := bf(app.Backend, machineName(app.Name))
			if err != nil {
				return err
			}
			// Passive: resolve the jail transport, never provision.
			if err := b.ResolveRunUser(cmd.Context()); err != nil {
				return fmt.Errorf("attach: jail not up (%v) — run `lever up` first", err)
			}
			// state gives this client the controller PAT (minted by a prior
			// `lever apply`'s bootstrap-token step) via HubTokenSource, so the
			// attach verb authenticates against the real, dev-auth-off hub;
			// AttachArgv (see internal/scion/lifecycle.go) embeds the resolved
			// token into the exec'd argv itself, since the attach path bypasses
			// this client's own env().
			state := brokerctl.StateDir(filepath.Dir(cfgPath))
			sc := scion.New(b.JailRunner(), scion.Options{
				HubEndpoint:    "http://127.0.0.1:8080",
				HubTokenSource: func() string { t, _ := state.LoadControllerPAT(); return t },
			})
			slug, project, err := attachTarget(app, b.MountDest(), argOrEmpty(args))
			if err != nil {
				return err
			}
			return execAttach(b, sc, slug, project)
		},
	}
}
