package jail

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/proc"
)

// ContainerState is podman's own view of one container: its full id, its
// name and its state.
type ContainerState struct {
	ID      string
	Name    string
	Status  string // podman's State.Status: created, running, paused, exited, ...
	Running bool
	Paused  bool
}

// Removable reports whether podman's state is one with no process in the
// container: created, configured, initialized, exited or stopped. Every other
// state, and any state with Running or Paused set, is not.
func (s ContainerState) Removable() bool {
	if s.Running || s.Paused {
		return false
	}
	return slices.Contains([]string{"created", "configured", "initialized", "exited", "stopped"}, s.Status)
}

var containerIDRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// containerStateOutputLimit caps podman's output for one inspect or rm. A
// caller may poll these many times while the agent's container still exists,
// and podman's real answer is one short line.
const containerStateOutputLimit = 64 << 10

// InspectContainerState reads ref's state from podman. It returns
// ErrNoContainer when podman knows no such container, and refuses an answer
// whose name is not ref, so a reference podman resolved some other way (an id
// prefix) is never acted on.
func InspectContainerState(ctx context.Context, r proc.Runner, ref string) (ContainerState, error) {
	if err := checkRef(ref); err != nil {
		return ContainerState{}, fmt.Errorf("inspecting container state: %w", err)
	}
	ctx, cancel := context.WithTimeout(proc.WithOutputLimit(ctx, containerStateOutputLimit), containerStateTimeout)
	defer cancel()
	res, err := r.Run(ctx, nil, "podman", "inspect", "--type", "container", "--format", "{{.Id}} {{.Name}} {{.State.Status}} {{.State.Running}} {{.State.Paused}}", ref)
	if err != nil {
		if noSuchContainer(res.Stderr) {
			return ContainerState{}, fmt.Errorf("inspecting container %s: %w", ref, ErrNoContainer)
		}
		return ContainerState{}, fmt.Errorf("inspecting container %s state: %w: %s", ref, err, podmanErrTail(res.Stderr))
	}
	f := strings.Fields(res.Stdout)
	if len(f) != 5 || !containerIDRE.MatchString(f[0]) || f[1] != ref {
		return ContainerState{}, fmt.Errorf("inspecting container %s state: unexpected answer", ref)
	}
	running, rerr := strconv.ParseBool(f[3])
	paused, perr := strconv.ParseBool(f[4])
	if rerr != nil || perr != nil {
		return ContainerState{}, fmt.Errorf("inspecting container %s state: unexpected answer", ref)
	}
	return ContainerState{ID: f[0], Name: f[1], Status: strings.ToLower(f[2]), Running: running, Paused: paused}, nil
}

// RemoveStoppedContainer removes the container with full id id. It never
// forces: podman refuses to remove a running or paused container, which is
// the last guard against removing one that started since it was inspected.
// A container already gone is not an error.
func RemoveStoppedContainer(ctx context.Context, r proc.Runner, id string) error {
	if !containerIDRE.MatchString(id) {
		return fmt.Errorf("removing container: invalid container id %q", id)
	}
	ctx, cancel := context.WithTimeout(proc.WithOutputLimit(ctx, containerStateOutputLimit), 2*time.Minute)
	defer cancel()
	res, err := r.Run(ctx, nil, "podman", "rm", id)
	if err != nil {
		if noSuchContainer(res.Stderr) {
			return nil
		}
		return fmt.Errorf("removing container %.12s: %w: %s", id, err, podmanErrTail(res.Stderr))
	}
	return nil
}

// ContainerJanitor runs the functions above through R, podman as the jail
// user.
type ContainerJanitor struct{ R proc.Runner }

func (j ContainerJanitor) State(ctx context.Context, ref string) (ContainerState, error) {
	return InspectContainerState(ctx, j.R, ref)
}

func (j ContainerJanitor) Remove(ctx context.Context, id string) error {
	return RemoveStoppedContainer(ctx, j.R, id)
}
