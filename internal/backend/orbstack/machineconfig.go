package orbstack

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/backend/guest"
)

// machineInfo is the part of `orb info --format json <machine>` the
// containment check reads. Pointers tell a field OrbStack left out from a
// false or empty one. OrbStack leaves "mounts" out when a machine has none.
type machineInfo struct {
	Record *struct {
		Config *struct {
			Isolated        *bool      `json:"isolated"`
			ForwardSSHAgent *bool      `json:"forward_ssh_agent"`
			Mounts          []orbMount `json:"mounts"`
		} `json:"config"`
	} `json:"record"`
}

type orbMount struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// containmentMismatch names what differs from the machine `orb create
// --isolated --mount <tree>:/lever` makes, or "" when nothing does. A
// machine whose config lacks the isolation field cannot be judged, so it
// counts as a mismatch: the check fails closed.
func (mi machineInfo) containmentMismatch(projectTree string) string {
	if mi.Record == nil || mi.Record.Config == nil || mi.Record.Config.Isolated == nil {
		return "orb info reports no isolation setting"
	}
	c := mi.Record.Config
	var bad []string
	if !*c.Isolated {
		bad = append(bad, "not isolated (it can read the Mac's files)")
	}
	if c.ForwardSSHAgent != nil && *c.ForwardSSHAgent {
		bad = append(bad, "forwards the Mac's SSH agent")
	}
	switch {
	case len(c.Mounts) == 0:
		bad = append(bad, "no project tree mount at "+common.MountDest)
	case len(c.Mounts) > 1:
		bad = append(bad, fmt.Sprintf("%d host mounts, want only the project tree at %s", len(c.Mounts), common.MountDest))
	case c.Mounts[0].Destination != common.MountDest:
		bad = append(bad, fmt.Sprintf("its host mount is at %s, not %s", c.Mounts[0].Destination, common.MountDest))
	case !samePath(c.Mounts[0].Source, projectTree):
		bad = append(bad, fmt.Sprintf("%s mounts %s, not the project tree %s", common.MountDest, c.Mounts[0].Source, projectTree))
	}
	return strings.Join(bad, "; ")
}

// samePath compares two host paths as written, then with symbolic links
// resolved, so a tree reached through a link still matches.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

func mismatchError(machine, detail string) error {
	return fmt.Errorf("orbstack machine %q exists with a mismatched containment config (%s); run 'lever down' then 'lever up' to recreate", machine, detail)
}

// verifyMachineConfig fails closed unless OrbStack's own record of the
// machine matches the containment lever creates: isolated, no SSH agent
// forwarding, and exactly the project tree mounted at /lever.
func (o *OrbStack) verifyMachineConfig(ctx context.Context, projectTree string) error {
	res, err := o.Runner().Run(ctx, nil, "orb", "info", "--format", "json", o.Machine())
	if err != nil {
		return fmt.Errorf("read the config of orbstack machine %q: %w", o.Machine(), err)
	}
	var mi machineInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &mi); err != nil {
		return mismatchError(o.Machine(), fmt.Sprintf("orb info output does not parse: %v", err))
	}
	if detail := mi.containmentMismatch(projectTree); detail != "" {
		return mismatchError(o.Machine(), detail)
	}
	return nil
}

// verifyGuestMounts is the second layer, run in the running machine: the
// Mac's /Users is absent and every host (virtiofs) mount is the project
// tree at /lever or below it. The guest cannot see a mount's host path;
// verifyMachineConfig covers that.
func (o *OrbStack) verifyGuestMounts(ctx context.Context) error {
	res, err := o.Runner().Run(guest.LimitOutput(ctx), nil, "orb", "-m", o.Machine(), "cat", "/proc/mounts")
	if err != nil {
		return fmt.Errorf("read the mounts of orbstack machine %q: %w", o.Machine(), err)
	}
	if detail := guestMountMismatch(res.Stdout); detail != "" {
		return mismatchError(o.Machine(), detail)
	}
	res, err = o.Runner().Run(ctx, nil, "orb", "-m", o.Machine(), "sh", "-c", "if [ -e /Users ]; then echo present; else echo absent; fi")
	if err != nil {
		return fmt.Errorf("look for /Users in orbstack machine %q: %w", o.Machine(), err)
	}
	switch strings.TrimSpace(res.Stdout) {
	case "absent":
		return nil
	case "present":
		return mismatchError(o.Machine(), "the Mac's /Users is visible in the machine")
	default:
		return mismatchError(o.Machine(), "cannot tell whether the Mac's /Users is visible in the machine")
	}
}

// guestMountMismatch reads /proc/mounts and names a host mount outside
// /lever, or a missing /lever, or "" when neither.
func guestMountMismatch(procMounts string) string {
	var sawTree bool
	var outside []string
	for _, line := range strings.Split(procMounts, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[2] != "virtiofs" {
			continue
		}
		mp := f[1]
		switch {
		case mp == common.MountDest:
			sawTree = true
		case strings.HasPrefix(mp, common.MountDest+"/"):
		default:
			outside = append(outside, mp)
		}
	}
	if len(outside) > 0 {
		return "host mounts outside " + common.MountDest + ": " + strings.Join(outside, ", ")
	}
	if !sawTree {
		return "no host mount at " + common.MountDest + " in the machine"
	}
	return ""
}
