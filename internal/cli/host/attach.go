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
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
)

// attachTarget resolves NAME ("" = manager) to a scion slug. The manager and
// every worker are agents in the one instance project (mountDest), so only the
// slug differs. An unknown name errors with the valid targets.
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

// attachArgv builds the host argv that attaches to slug. A controller PAT is
// staged in the guest first (0600 in the run user's runtime dir) and read
// there, so it never enters the host command line, which `ps` shows to every
// local user for the whole session.
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

var detachOtherClients = func(ctx context.Context, r proc.Runner, ref string) (int, error) {
	return jail.AgentProbe{R: r}.DetachClients(ctx, ref)
}

// detachOthersFlagUsage is the --detach-others flag's help, on attach and up.
const detachOthersFlagUsage = "first detach the other terminals attached to the agent (by default they stay attached); use it when a stale attach froze the view"

// detachBeforeAttach detaches the tmux clients earlier attaches left on slug's
// session, when detachOthers is set: a stale client from a vanished terminal
// can freeze the view or keep the old size. By default nothing is detached, so
// a second terminal can attach alongside. Like `tmux attach -d` (scion's
// attach has no such flag) but sparing the container's own client.
// Best-effort: a failure is not reported (the attach that follows reports a
// missing container).
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

// prepareAttach is the part of an attach that changes the jail: it detaches
// the other terminals (detachOthers) and stages the token, and returns the
// argv that attaches.
func prepareAttach(ctx context.Context, w io.Writer, b backend.Backend, sc *scion.Client, slug, project string, detachOthers bool) ([]string, error) {
	detachBeforeAttach(ctx, w, b.JailRunner(), slug, project, detachOthers)
	return attachArgv(ctx, b, sc, slug, project)
}

// attachChild runs argv on the operator's terminal. The child stays in lever's
// process group, which is the terminal's foreground group, so it keeps the
// controlling terminal, gets SIGWINCH on a resize and stops with lever on a
// job-control stop. Meanwhile the terminal's interrupt and quit belong to the
// attach (the terminal sends them to it too), so lever waits them out;
// terminate and hangup sent to lever are passed on. They are caught, not
// ignored, because an ignored signal would be inherited by the child; one that
// lever itself inherited as ignored (nohup) is left so, for the child to
// inherit too. However the child ends, the terminal's line settings and modes
// are restored after.
func attachChild(argv []string, in, out, errw *os.File) error {
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	c := &exec.Cmd{Path: bin, Args: argv}
	// A nil *os.File in the interface fields would not read as unset.
	if in != nil {
		c.Stdin = in
	}
	if out != nil {
		c.Stdout = out
	}
	if errw != nil {
		c.Stderr = errw
	}
	sigs := make(chan os.Signal, 4)
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(sig) {
			signal.Notify(sigs, sig)
		}
	}
	defer signal.Stop(sigs)
	term := saveAttachTerm(in, out)
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
			term.restore(attachEndedAbnormally(err))
			if err != nil {
				return fmt.Errorf("attach: %w", err)
			}
			return nil
		}
	}
}

// withAttachExitCode makes `lever attach` exit with the status a shell would
// report for the attach itself.
func withAttachExitCode(err error) error {
	if code, ok := attachExitCode(err); ok && code > 0 {
		return cli.WithExitCode(err, code)
	}
	return err
}

// attachLockWait is attach's default --wait: an operator who attaches
// during a bring-up or a heal is let in once it is done.
const attachLockWait = 2 * time.Minute

// newAttachCmd attaches to a running agent and does no lifecycle work. It is
// passive: a jail that is not up fails ResolveRunUser rather than being
// provisioned; an agent that is not running surfaces scion's own error.
func newAttachCmd(bf BackendFactory) *cobra.Command {
	var detachOthers bool
	var lockWait time.Duration
	c := &cobra.Command{
		Use:   "attach [NAME]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Attach your TTY to the manager (default) or a named worker agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Config comes from the CWD: the positional is the agent name.
			app, state, err := loadAppAndState(nil)
			if err != nil {
				return err
			}
			b, err := bf(app.Backend, machineName(app.Name))
			if err != nil {
				return err
			}
			if err := b.ResolveRunUser(cmd.Context()); err != nil {
				return fmt.Errorf("attach: %w (%v) — run `lever up` first", errJailNotUp, err)
			}
			// HubTokenSource gives the client the controller PAT.
			// attachArgv stages it in the guest, since the attach's argv is public.
			sc := brokerctl.HostScionClient(b.JailRunner(), state, app.Scion.AgentRole)
			slug, project, err := attachTarget(app, b.MountDest(), argOrEmpty(args))
			if err != nil {
				return err
			}
			// Staging the token and detaching terminals change the jail, so they
			// wait out a bring-up or a heal; the attach itself, which can last all
			// day, holds nothing.
			unlock, err := lockLifecycle(cmd.Context(), app, state, "lever attach", lockWait, lockNotice(cmd))
			if err != nil {
				return err
			}
			argv, err := prepareAttach(cmd.Context(), cmd.ErrOrStderr(), b, sc, slug, project, detachOthers)
			unlock()
			if err != nil {
				return err
			}
			return withAttachExitCode(attachChild(argv, os.Stdin, os.Stdout, os.Stderr))
		},
	}
	c.Flags().BoolVar(&detachOthers, "detach-others", false, detachOthersFlagUsage)
	addLockWaitFlag(c, &lockWait, attachLockWait)
	return c
}
