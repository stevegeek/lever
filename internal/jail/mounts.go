package jail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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

// envKeyRE is the shape of an environment variable name ContainerEnvValue
// will look up; it is interpolated into a guest-side grep pattern.
var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// containerEnvScript prints the one `KEY=value` line of a container's
// creation env that names $2, or nothing. The filtering happens IN THE GUEST
// on purpose: an agent container's env carries credentials, and none of it
// but the requested line may cross back to the host (or into an error).
// podman's own failure keeps its exit status and stderr, so a missing
// container still reads as ErrNoContainer.
const containerEnvScript = `out=$(podman inspect --type container --format '{{range .Config.Env}}{{println .}}{{end}}' "$1") || exit $?
printf '%s\n' "$out" | grep -E "^$2=" | head -n 1
exit 0`

// ContainerEnvValue reads one variable from the creation env of the jail
// container named by ref (id or name). set is false when the container has no
// such variable. Only that one variable ever leaves the guest.
func ContainerEnvValue(ctx context.Context, r proc.Runner, ref, key string) (value string, set bool, err error) {
	if strings.TrimSpace(ref) == "" || strings.HasPrefix(ref, "-") {
		return "", false, fmt.Errorf("inspecting container env: invalid container reference %q", ref)
	}
	if !envKeyRE.MatchString(key) {
		return "", false, fmt.Errorf("inspecting container env: invalid variable name %q", key)
	}
	res, err := r.Run(ctx, nil, "sh", "-c", containerEnvScript, "sh", ref, key)
	if err != nil {
		if s := strings.ToLower(res.Stderr); strings.Contains(s, "no such container") || strings.Contains(s, "no such object") {
			return "", false, fmt.Errorf("inspecting container %s: %w", ref, ErrNoContainer)
		}
		return "", false, fmt.Errorf("inspecting container %s env: %w: %s", ref, err, strings.TrimSpace(res.Stderr))
	}
	line := strings.TrimSpace(res.Stdout)
	if line == "" {
		return "", false, nil
	}
	v, ok := strings.CutPrefix(line, key+"=")
	if !ok {
		return "", false, fmt.Errorf("inspecting container %s env: unexpected output for %s", ref, key)
	}
	return v, true, nil
}
