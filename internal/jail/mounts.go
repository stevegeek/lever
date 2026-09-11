package jail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

// ErrNoContainer reports that podman knows no container by the given
// reference: the record has no container at the moment (never started, or
// removed), which is not an inspect failure.
var ErrNoContainer = errors.New("no such container")

// ContainerMountTargets returns the in-container mount points of the jail
// container named by ref — an id or a name (podman inspect, through r).
// scion's listing carries neither the mounts nor, on every pin, the id
// (89ed0fe8 reports an empty containerId), so doctor falls back to scion's
// container name, <project>--<agent>. Doctor uses this to tell a worker
// created before the guest ticket channel (no /run/lever mount) from one
// that will boot.
func ContainerMountTargets(ctx context.Context, r proc.Runner, ref string) ([]string, error) {
	if strings.TrimSpace(ref) == "" || strings.HasPrefix(ref, "-") {
		return nil, fmt.Errorf("inspecting container mounts: invalid container reference %q", ref)
	}
	res, err := r.Run(ctx, nil, "podman", "inspect", "--type", "container", "--format", "{{json .Mounts}}", ref)
	if err != nil {
		if s := strings.ToLower(res.Stderr); strings.Contains(s, "no such container") || strings.Contains(s, "no such object") {
			return nil, fmt.Errorf("inspecting container %s: %w", ref, ErrNoContainer)
		}
		return nil, fmt.Errorf("inspecting container %s mounts: %w: %s", ref, err, strings.TrimSpace(res.Stderr))
	}
	var mounts []struct {
		Destination string `json:"Destination"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &mounts); err != nil {
		return nil, fmt.Errorf("inspecting container %s mounts: parse: %w", ref, err)
	}
	targets := make([]string, 0, len(mounts))
	for _, m := range mounts {
		targets = append(targets, m.Destination)
	}
	return targets, nil
}

// ContainerName is the name scion gives an agent's container: the hub
// project name, two dashes, the agent slug (scion pkg/agent/run.go
// containerName). project is the hub project key (the mount dest's base).
func ContainerName(project, agent string) string {
	return project + "--" + agent
}
