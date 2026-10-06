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

// Mount is one mount of a jail container as podman inspect reports it: the
// guest-side source, the in-container destination and whether it is
// writable.
type Mount struct {
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

// ContainerMounts returns the mounts of the jail container named by ref — an
// id or a name (podman inspect, through r). scion's listing carries neither
// the mounts nor, on every pin, the id (89ed0fe8 reports an empty
// containerId), so doctor falls back to scion's container name,
// <project>--<agent>. Doctor uses this to tell a manager created before
// manager.read_only named a path (no read-only mount there) from one that
// holds it read-only.
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
// points. Doctor uses it to tell a worker created before the guest ticket
// channel (no /run/lever mount) from one that will boot.
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

// ContainerPathWritable asks the running jail container named by ref
// whether target is writable (`podman exec --user 0 <ref> test -w
// <target>`, argv only, no shell): as root, so only a read-only mount says
// no. It is the live half of the
// manager.read_only check: an inspect still lists a read-only mount after
// the host replaced the directory it covered (a rename-based deploy, rm -rf
// and recreate), while a write from the container lands in the new,
// unprotected directory — verified on OrbStack 2026-10-04. `test` answers
// exit 1 with no output for "not writable" (EROFS on a read-only mount);
// anything else — podman's own failure, a stopped container, a missing
// `test` — is an error, never "not writable", so a probe that could not run
// cannot read as protected.
func ContainerPathWritable(ctx context.Context, r proc.Runner, ref, target string) (bool, error) {
	if strings.TrimSpace(ref) == "" || strings.HasPrefix(ref, "-") {
		return false, fmt.Errorf("probing container path: invalid container reference %q", ref)
	}
	if !strings.HasPrefix(target, "/") {
		return false, fmt.Errorf("probing container path: target %q is not absolute", target)
	}
	// --user 0: the question is "is this a read-only mount", not "may the
	// agent user write here". root bypasses permission bits but still gets
	// EROFS on a read-only mount, so the answer does not depend on the
	// image's USER or the directory's mode.
	res, err := r.Run(ctx, nil, "podman", "exec", "--user", "0", ref, "test", "-w", target)
	quiet := strings.TrimSpace(res.Stdout) == "" && benignStderr(res.Stderr)
	switch {
	case err == nil && res.Code == 0:
		return true, nil
	case res.Code == 1 && quiet:
		return false, nil
	}
	if s := strings.ToLower(res.Stderr); strings.Contains(s, "no such container") || strings.Contains(s, "no such object") {
		return false, fmt.Errorf("probing container %s: %w", ref, ErrNoContainer)
	}
	if err == nil {
		err = fmt.Errorf("exit status %d", res.Code)
	}
	return false, fmt.Errorf("probing %s in container %s: %w: %s", target, ref, err, strings.TrimSpace(res.Stderr))
}

// benignStderr reports whether stderr holds nothing but podman's own
// warning lines — `WARN[0000] ...` (the default text format) or a logrus
// `level=warning` line — which podman prints on some guests for every
// command (a cgroups or config notice). Any other line keeps the probe an
// error: a stderr lever cannot read never lets a result count as "not
// writable", so it never reads as protected.
func benignStderr(stderr string) bool {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "WARN[") ||
			(strings.HasPrefix(line, "time=") && strings.Contains(line, " level=warning ")) {
			continue
		}
		return false
	}
	return true
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
