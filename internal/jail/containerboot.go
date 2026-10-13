package jail

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

// ContainerStartedBeforeBoot reports whether podman last started the
// container ref before the guest's current boot: its State.StartedAt is
// earlier than the boot time /proc/stat names (btime). A container that
// ran in an earlier boot and has not started in this one answers true.
//
// It is the evidence of a VM restart when lever kept no boot id for the
// manager (lever#179: an instance whose last bring-up predates the
// record). Both times come from the guest's own clock. A container podman
// never started (a zero StartedAt) answers false, as does any answer that
// does not parse: the caller then leaves the record alone.
//
// btime is the realtime clock minus the time since boot, so a step of the
// guest clock after boot moves it. That is common, not rare: a host that
// slept pauses the guest, and the guest clock is stepped forward on wake,
// moving btime later by the length of the sleep. A container started
// before such a step in the same boot then reads as older than the boot.
// Callers act only on a container podman says is not running, where the
// remedy (suspend and resume, keeping the conversation) is the one the
// operator would run by hand, so the error is benign. On OrbStack btime is
// the shared VM's, so a restart of the machine alone is not seen (a false
// no, which is safe).
func ContainerStartedBeforeBoot(ctx context.Context, r proc.Runner, ref string) (bool, error) {
	if strings.TrimSpace(ref) == "" || strings.HasPrefix(ref, "-") {
		return false, fmt.Errorf("inspecting container start: invalid container reference %q", ref)
	}
	ctx, cancel := context.WithTimeout(ctx, containerStateTimeout)
	defer cancel()
	res, err := r.Run(ctx, nil, "podman", "inspect", "--type", "container", "--format", "{{.State.StartedAt.Unix}}", ref)
	if err != nil {
		return false, fmt.Errorf("inspecting container %s start: %w: %s", ref, err, strings.TrimSpace(res.Stderr))
	}
	started, err := strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64)
	if err != nil {
		return false, fmt.Errorf("inspecting container %s start: unexpected answer", ref)
	}
	if started <= 0 {
		return false, nil // never started (Go's zero time is far before 1970)
	}
	stat, err := r.Run(ctx, nil, "cat", "/proc/stat")
	if err != nil {
		return false, fmt.Errorf("reading the guest boot time: %w", err)
	}
	btime, ok := parseBtime(stat.Stdout)
	if !ok {
		return false, fmt.Errorf("reading the guest boot time: no btime line")
	}
	return started < btime, nil
}

// parseBtime returns the "btime <seconds>" value of /proc/stat.
func parseBtime(stat string) (int64, bool) {
	for line := range strings.Lines(stat) {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "btime" {
			v, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil || v <= 0 {
				return 0, false
			}
			return v, true
		}
	}
	return 0, false
}
