package jail

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

// ContainerMountTargets returns the in-container mount points of the jail
// container id (podman inspect, through r). scion's listing carries the id
// but not the mounts; doctor uses this to tell a worker created before the
// guest ticket channel (no /run/lever mount) from one that will boot.
func ContainerMountTargets(ctx context.Context, r proc.Runner, containerID string) ([]string, error) {
	if strings.TrimSpace(containerID) == "" || strings.HasPrefix(containerID, "-") {
		return nil, fmt.Errorf("inspecting container mounts: invalid container id %q", containerID)
	}
	res, err := r.Run(ctx, nil, "podman", "inspect", "--type", "container", "--format", "{{json .Mounts}}", containerID)
	if err != nil {
		return nil, fmt.Errorf("inspecting container %s mounts: %w: %s", containerID, err, strings.TrimSpace(res.Stderr))
	}
	var mounts []struct {
		Destination string `json:"Destination"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &mounts); err != nil {
		return nil, fmt.Errorf("inspecting container %s mounts: parse: %w", containerID, err)
	}
	targets := make([]string, 0, len(mounts))
	for _, m := range mounts {
		targets = append(targets, m.Destination)
	}
	return targets, nil
}
