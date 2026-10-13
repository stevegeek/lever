package host

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
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

// detachOtherClients detaches the tmux clients attached to an agent's
// session (jail.AgentProbe.DetachClients); a test seam.
var detachOtherClients = func(ctx context.Context, r proc.Runner, ref string) (int, error) {
	return jail.AgentProbe{R: r}.DetachClients(ctx, ref)
}

// detachOthersFlagUsage is the --detach-others flag's help, on attach and up.
const detachOthersFlagUsage = "first detach the other terminals attached to the agent (by default they stay attached); use it when a stale attach froze the view"

// detachBeforeAttach detaches the tmux clients earlier attaches left on
// slug's session before the attach, when detachOthers is set (lever#170,
// the --detach-others flag): an earlier attach whose terminal went away
// stays attached as a stale client, and the new view can then freeze or
// keep the old terminal's size. By default nothing is detached, so a
// second terminal can attach without taking the first one's view away.
// What `tmux attach -d` does (scion's attach has no such flag), except that
// the container's own client is spared (jail.AgentProbe.DetachClients).
// Best-effort: a failure is not reported (scion's attach that follows
// reports a container that is not there), and a count of detached clients
// is printed to w.
func detachBeforeAttach(ctx context.Context, w io.Writer, r proc.Runner, slug, project string, detachOthers bool) {
	if !detachOthers {
		return
	}
	n, err := detachOtherClients(ctx, r, jail.ContainerName(path.Base(project), slug))
	if err != nil || n == 0 {
		return
	}
	noun := "terminal"
	if n > 1 {
		noun = "terminals"
	}
	fmt.Fprintf(w, "lever: detached %d other %s attached to %q (--detach-others)\n", n, noun, slug)
}

// execAttach replaces the current process with the backend-wrapped scion attach
// for slug in project — the same TTY-handover chain `lever up` uses. It only
// returns on error (syscall.Exec does not return on success). With
// detachOthers, other terminals attached to the agent are detached first
// (detachBeforeAttach).
func execAttach(ctx context.Context, w io.Writer, b backend.Backend, sc *scion.Client, slug, project string, detachOthers bool) error {
	detachBeforeAttach(ctx, w, b.JailRunner(), slug, project, detachOthers)
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

// runAttach runs the backend-wrapped scion attach for slug in project as a
// child on the operator's terminal and returns when it ends: the operator
// detached, or the session ended. `lever up` on a terminal uses it instead
// of execAttach so it can check the manager after the detach (lever#188).
// While the child runs, the terminal's interrupt and quit signals are the
// attach's (the terminal sends them to it too): lever waits them out
// rather than dying under it. A terminate or hangup sent to lever is
// passed on to the attach, which then ends, as exec'ing it did. (The child
// gets the default handling back on exec; only an ignored signal would be
// inherited, so they are caught, not ignored.) With detachOthers, other
// terminals attached to the agent are detached first (detachBeforeAttach).
func runAttach(ctx context.Context, w io.Writer, b backend.Backend, sc *scion.Client, slug, project string, detachOthers bool) error {
	detachBeforeAttach(ctx, w, b.JailRunner(), slug, project, detachOthers)
	argv, err := attachArgv(ctx, b, sc, slug, project)
	if err != nil {
		return err
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	c := exec.Command(bin, argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	if err := c.Start(); err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGTERM || sig == syscall.SIGHUP {
				_ = c.Process.Signal(sig)
			}
		case err := <-done:
			if err != nil {
				return fmt.Errorf("attach: %w", err)
			}
			return nil
		}
	}
}

// newAttachCmd is a debugging/eyes-on verb: it attaches to a RUNNING agent and
// deliberately does no lifecycle work (bring things up with `lever up`). It is
// strictly passive: if the jail itself is not up, ResolveRunUser fails fast
// rather than provisioning it. If the jail is up but the target agent/worker is
// not running, scion's own attach error surfaces.
func newAttachCmd(bf BackendFactory) *cobra.Command {
	var detachOthers bool
	c := &cobra.Command{
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
			return execAttach(cmd.Context(), cmd.ErrOrStderr(), b, sc, slug, project, detachOthers)
		},
	}
	c.Flags().BoolVar(&detachOthers, "detach-others", false, detachOthersFlagUsage)
	return c
}
