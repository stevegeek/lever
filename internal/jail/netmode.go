package jail

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/proc"
)

// ContainerNetworkMode returns the network mode podman created the jail
// container named by ref with (HostConfig.NetworkMode: "pasta",
// "slirp4netns", "host", ...), through r. ErrNoContainer when there is
// none. The mode is fixed at create, so a container made before lever's
// pasta drop-in took effect keeps slirp4netns until it is recreated
// (lever#35).
func ContainerNetworkMode(ctx context.Context, r proc.Runner, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" || strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("inspecting container network: invalid container reference %q", ref)
	}
	res, err := r.Run(ctx, nil, "podman", "inspect", "--type", "container", "--format", "{{.HostConfig.NetworkMode}}", ref)
	if err != nil {
		if s := strings.ToLower(res.Stderr); strings.Contains(s, "no such container") || strings.Contains(s, "no such object") {
			return "", fmt.Errorf("inspecting container %s: %w", ref, ErrNoContainer)
		}
		return "", fmt.Errorf("inspecting container %s network: %w: %s", ref, err, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// containerStateTimeout bounds ContainerRunning's podman inspect.
var containerStateTimeout = 10 * time.Second

// ContainerRunning reports whether the jail container named by ref is
// running or paused (podman's State, through r): false, with no error, when
// podman knows no such container. Unlike the hub's agent record, which the
// agent can update about itself, this is the runtime's own view.
func ContainerRunning(ctx context.Context, r proc.Runner, ref string) (bool, error) {
	if strings.TrimSpace(ref) == "" || strings.HasPrefix(ref, "-") {
		return false, fmt.Errorf("inspecting container state: invalid container reference %q", ref)
	}
	// Its own bound: the caller holds the worker lock, so a hung podman
	// must not hold it until the client goes away.
	ctx, cancel := context.WithTimeout(ctx, containerStateTimeout)
	defer cancel()
	res, err := r.Run(ctx, nil, "podman", "inspect", "--type", "container", "--format", "{{.State.Running}} {{.State.Paused}}", ref)
	if err != nil {
		if s := strings.ToLower(res.Stderr); strings.Contains(s, "no such container") || strings.Contains(s, "no such object") {
			return false, nil
		}
		return false, fmt.Errorf("inspecting container %s state: %w: %s", ref, err, strings.TrimSpace(res.Stderr))
	}
	switch strings.TrimSpace(res.Stdout) {
	case "false false":
		return false, nil
	case "true false", "true true", "false true":
		return true, nil
	}
	return false, fmt.Errorf("inspecting container %s state: unexpected answer %q", ref, strings.TrimSpace(res.Stdout))
}
