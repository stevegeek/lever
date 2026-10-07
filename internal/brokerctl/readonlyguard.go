package brokerctl

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
)

// guardError is a ReadOnlyGuard refusal: fixed text lever composes (never
// container or guest output, which the agent can shape), and a reason
// class the supervisor logs once per change.
type guardError struct{ class, msg string }

func (e *guardError) Error() string { return e.msg }

// guardClass is the class of a guard refusal for the supervisor's
// once-per-reason log: a guardError's own, else one class for all.
func guardClass(err error) string {
	var g *guardError
	if errors.As(err, &g) {
		return g.class
	}
	return "other"
}

// readOnlyGuard is the production ReadOnlyGuard. Through the jail runner it
// reads the manager container's configured mounts (podman inspect) and
// requires the whole manager.read_only plan (apply.ManagerTreeMountGaps:
// entries read-only, pins in place — a missing pin lets the agent move an
// entry away). Then it checks the RUNNING container from the guest side
// (jail.ContainerLiveMounts: the kernel's mountinfo and device+inode, no
// program run inside the container, where the agent is root): every
// planned directory must be a mount point, the entries read-only, each
// covering the directory the host has there now. Anything it cannot read
// — no container yet, a stopped one — is a refusal, so the tool waits.
func readOnlyGuard(app *config.App, jr proc.Runner, jailMount string) ReadOnlyGuard {
	ref := jail.ContainerName(path.Base(jailMount), app.Name)
	want := app.ManagerTreeMounts()
	return func(ctx context.Context, _ []string) error {
		return checkManagerReadOnly(ctx, app.Name, jailMount, want,
			func(ctx context.Context) ([]jail.Mount, error) { return jail.ContainerMounts(ctx, jr, ref) },
			func(ctx context.Context, live []jail.LiveMount) ([]jail.LiveMountProblem, error) {
				return jail.ContainerLiveMounts(ctx, jr, ref, live)
			})
	}
}

// checkManagerReadOnly is readOnlyGuard's decision, with the two reads
// injected for tests.
func checkManagerReadOnly(ctx context.Context, name, jailMount string, want []config.TreeMount,
	mounts func(context.Context) ([]jail.Mount, error),
	live func(context.Context, []jail.LiveMount) ([]jail.LiveMountProblem, error)) error {
	got, err := mounts(ctx)
	if err != nil {
		if errors.Is(err, jail.ErrNoContainer) {
			return &guardError{"no-container", fmt.Sprintf("manager %q has no container yet", name)}
		}
		return &guardError{"mounts-unreadable", fmt.Sprintf("lever cannot read manager %q's mounts", name)}
	}
	if gaps := apply.ManagerTreeMountGaps(jailMount, want, got); !gaps.Empty() {
		return &guardError{"gaps:" + gaps.String(), fmt.Sprintf("manager %q does not hold that protection: %s", name, gaps)}
	}
	plan := make([]jail.LiveMount, 0, len(want))
	for _, w := range want {
		plan = append(plan, jail.LiveMount{Target: path.Join(scion.ContainerWorkspace, w.Rel), Source: path.Join(jailMount, w.Rel), ReadOnly: w.ReadOnly})
	}
	problems, err := live(ctx, plan)
	switch {
	case errors.Is(err, jail.ErrContainerNotRunning), errors.Is(err, jail.ErrNoContainer):
		return &guardError{"not-running", fmt.Sprintf("manager %q is not running, so lever cannot see its live mounts", name)}
	case err != nil:
		return &guardError{"live-unreadable", fmt.Sprintf("lever cannot read manager %q's live mounts from the guest", name)}
	case len(problems) > 0:
		parts := make([]string, 0, len(problems))
		for _, p := range problems {
			parts = append(parts, strings.TrimPrefix(p.Target, scion.ContainerWorkspace+"/")+" "+p.Reason)
		}
		msg := strings.Join(parts, "; ")
		return &guardError{"live:" + msg, fmt.Sprintf("manager %q does not hold that protection: %s", name, msg)}
	}
	return nil
}
