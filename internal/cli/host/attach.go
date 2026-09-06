package host

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
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

// attachArgv builds the host argv that attaches to slug in project. When the
// client holds a controller PAT it is staged in the guest first (a 0600 file
// in the run user's runtime dir) and the inner command is wrapped to read it
// there, so the PAT never enters the host command line — which `ps` shows to
// every local user for the whole attach session. Without a token the argv is
// the plain backend-wrapped scion attach.
func attachArgv(ctx context.Context, b backend.Backend, sc *scion.Client, slug, project string) ([]string, error) {
	inner := sc.AttachArgv(slug, project)
	if tok := sc.HubToken(); tok != "" {
		if err := jail.StageHubToken(ctx, b.JailRunner(), tok); err != nil {
			return nil, fmt.Errorf("attach: %w", err)
		}
		inner = jail.WithHubTokenFromFile(inner)
	}
	return b.AttachArgv(inner), nil
}

// execAttach replaces the current process with the backend-wrapped scion attach
// for slug in project — the same TTY-handover chain `lever up` uses. It only
// returns on error (syscall.Exec does not return on success).
func execAttach(ctx context.Context, b backend.Backend, sc *scion.Client, slug, project string) error {
	argv, err := attachArgv(ctx, b, sc, slug, project)
	if err != nil {
		return err
	}
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
			// Config is always discovered from the CWD (never a positional — the
			// positional is the agent NAME, resolved by attachTarget below).
			app, state, err := loadAppAndState(nil)
			if err != nil {
				return err
			}
			b, err := bf(app.Backend, machineName(app.Name))
			if err != nil {
				return err
			}
			// Passive: resolve the jail transport, never provision.
			if err := b.ResolveRunUser(cmd.Context()); err != nil {
				return fmt.Errorf("attach: %w (%v) — run `lever up` first", errJailNotUp, err)
			}
			// state gives this client the controller PAT (minted by a prior
			// `lever apply`'s bootstrap-token step) via HubTokenSource, so the
			// attach verb authenticates against the real, dev-auth-off hub;
			// attachArgv stages that token in the guest, since the attach path
			// bypasses this client's own env() and the exec'd argv is public.
			sc := brokerctl.HostScionClient(b.JailRunner(), state, app.Scion.AgentRole)
			slug, project, err := attachTarget(app, b.MountDest(), argOrEmpty(args))
			if err != nil {
				return err
			}
			return execAttach(cmd.Context(), b, sc, slug, project)
		},
	}
}
