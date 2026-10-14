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
// reference, which is not an inspect failure.
var ErrNoContainer = errors.New("no such container")

// Mount is one mount of a jail container as podman inspect reports it.
type Mount struct {
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

// ContainerMounts returns the mounts of the jail container named by ref, an
// id or a name. scion's listing carries neither the mounts nor, on some
// scion versions, the id, so callers may pass ContainerName.
func ContainerMounts(ctx context.Context, r proc.Runner, ref string) ([]Mount, error) {
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
	var mounts []Mount
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &mounts); err != nil {
		return nil, fmt.Errorf("inspecting container %s mounts: parse: %w", ref, err)
	}
	return mounts, nil
}

// ContainerMountTargets is ContainerMounts reduced to the in-container mount
// points.
func ContainerMountTargets(ctx context.Context, r proc.Runner, ref string) ([]string, error) {
	mounts, err := ContainerMounts(ctx, r, ref)
	if err != nil {
		return nil, err
	}
	targets := make([]string, 0, len(mounts))
	for _, m := range mounts {
		targets = append(targets, m.Destination)
	}
	return targets, nil
}

// ContainerHasCharDevice reports whether path is a character device inside
// the running jail container ref. The agent controls what `test` is in its
// container, so the call is bounded and only the exit code is read.
func ContainerHasCharDevice(ctx context.Context, r proc.Runner, ref, path string) (bool, error) {
	if strings.TrimSpace(ref) == "" || strings.HasPrefix(ref, "-") {
		return false, fmt.Errorf("probing container device: invalid container reference %q", ref)
	}
	if !strings.HasPrefix(path, "/") {
		return false, fmt.Errorf("probing container device: path %q is not absolute", path)
	}
	ctx, cancel := BoundAgentExec(ctx)
	defer cancel()
	res, err := runGuestBounded(ctx, r, "podman", "exec", "--user", "0", ref, "test", "-c", path)
	switch {
	case err == nil && res.Code == 0:
		return true, nil
	case res.Code == 1:
		return false, nil
	}
	if s := strings.ToLower(res.Stderr); strings.Contains(s, "no such container") || strings.Contains(s, "no such object") {
		return false, fmt.Errorf("probing container %s: %w", ref, ErrNoContainer)
	}
	if err == nil {
		err = fmt.Errorf("exit %d", res.Code)
	}
	return false, fmt.Errorf("probing container %s device %s: %w", ref, path, err)
}

// ContainerName is the name scion gives an agent's container (scion
// pkg/agent/run.go containerName). project is the hub project key.
func ContainerName(project, agent string) string {
	return project + "--" + agent
}

// envKeyRE guards a name that is interpolated into a guest-side grep pattern.
var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// containerEnvScript filters in the guest on purpose: an agent container's
// env carries credentials, and none of it but the requested line may cross
// back to the host (or into an error). podman's failure keeps its exit status
// and stderr, so a missing container still reads as ErrNoContainer.
const containerEnvScript = `out=$(podman inspect --type container --format '{{range .Config.Env}}{{println .}}{{end}}' "$1") || exit $?
printf '%s\n' "$out" | grep -E "^$2=" | head -n 1
exit 0`

// ContainerEnvValue reads one variable from the creation env of the jail
// container ref. Only that one variable ever leaves the guest.
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
