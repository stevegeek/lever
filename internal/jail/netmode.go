package jail

import (
	"context"
	"fmt"
	"strings"

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
