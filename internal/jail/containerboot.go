package jail

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

// ContainerStartedBeforeBoot reports whether podman last started the
// container ref before the guest's current boot (/proc/stat btime). It is
// the evidence of a VM restart when lever kept no boot id for the manager. A
// container podman never started answers false.
//
// btime is the realtime clock minus the uptime, and a host that slept steps
// the guest clock forward on wake, so a container started before the sleep
// in the same boot reads as older than the boot. Callers act only on a
// stopped container, where the remedy (suspend and resume) is benign. On
// OrbStack btime is the shared VM's, so a restart of the machine alone is
// not seen (a false no, which is safe).
func ContainerStartedBeforeBoot(ctx context.Context, r proc.Runner, ref string) (bool, error) {
	return beforeBoot(ctx, r, ref, "{{.State.StartedAt.Unix}}", "start")
}

// ContainerCreatedBeforeBoot reports whether podman created the container ref
// before the guest's current boot. A container podman still lists as created
// that was made in an ended boot is no start under way: the podman run that
// made it died with that boot, and nothing will start it. btime's caveat
// (ContainerStartedBeforeBoot) holds: a host sleep can make a container made
// in this boot read as older, and callers act only where the remedy (suspend
// and resume) is benign.
func ContainerCreatedBeforeBoot(ctx context.Context, r proc.Runner, ref string) (bool, error) {
	return beforeBoot(ctx, r, ref, "{{.Created.Unix}}", "creation")
}

func beforeBoot(ctx context.Context, r proc.Runner, ref, format, what string) (bool, error) {
	if strings.TrimSpace(ref) == "" || strings.HasPrefix(ref, "-") {
		return false, fmt.Errorf("inspecting container %s: invalid container reference %q", what, ref)
	}
	ctx, cancel := context.WithTimeout(ctx, containerStateTimeout)
	defer cancel()
	res, err := r.Run(ctx, nil, "podman", "inspect", "--type", "container", "--format", format, ref)
	if err != nil {
		return false, fmt.Errorf("inspecting container %s %s: %w: %s", ref, what, err, strings.TrimSpace(res.Stderr))
	}
	at, err := strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64)
	if err != nil {
		return false, fmt.Errorf("inspecting container %s %s: unexpected answer", ref, what)
	}
	if at <= 0 {
		return false, nil // never happened (Go's zero time is far before 1970)
	}
	stat, err := r.Run(ctx, nil, "cat", "/proc/stat")
	if err != nil {
		return false, fmt.Errorf("reading the guest boot time: %w", err)
	}
	btime, ok := parseBtime(stat.Stdout)
	if !ok {
		return false, fmt.Errorf("reading the guest boot time: no btime line")
	}
	return at < btime, nil
}

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
