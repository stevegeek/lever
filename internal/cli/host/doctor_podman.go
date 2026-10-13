package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/jail"
)

const podmanStorageCheck = "podman storage"

// podmanResetFix is the recovery for a damaged store: back up, reset it as the
// jail user, and bring the instance up on the same records (see the operations
// guide).
const podmanResetFix = "if `lever up` fails at load-image: back up first (the guest's ~/.scion with the hub database, and the agent homes), " +
	"then in the guest as the jail user run `podman system reset --force` (it removes every image, container and " +
	"podman volume in the jail; the hub records, agent homes and the tree stay), then `lever up` without --fresh; " +
	"see \"Recovering the jail's podman store\" in the operations guide"

// checkPodmanStorage reports the jail's rootless podman store: podman's
// corruption warning fails; unused or incomplete layers warn, since a load
// that reuses a damaged orphan layer fails. Read-only (bounded, as the run
// user): lever never deletes a layer; the fix says how to recover.
func checkPodmanStorage(ctx context.Context, read func(context.Context) (jail.StoreHealth, error)) checkResult {
	h, err := read(ctx)
	if err != nil {
		return warnResult(podmanStorageCheck, "not checked (could not read the jail's podman store): "+firstLine(err.Error()),
			"a podman that hangs or a store file too big to read can itself be a sign of damage: see \"Recovering the jail's podman store\" in the operations guide")
	}
	summary := fmt.Sprintf("podman %s, %s: %d %s, %d %s, %d %s", orUnknown(h.Version), h.Driver,
		h.Images, plural(h.Images, "image", "images"), h.Containers, plural(h.Containers, "container", "containers"),
		h.Layers, plural(h.Layers, "layer", "layers"))
	var problems []string
	if h.Orphans > 0 {
		problems = append(problems, fmt.Sprintf("%d %s no image or container uses (%s)", h.Orphans, plural(h.Orphans, "layer", "layers"), humanBytes(h.OrphanBytes)))
	}
	if len(h.Notices) > 0 {
		problems = append(problems, "podman notes: "+strings.Join(h.Notices, " / "))
	}
	if h.Incomplete > 0 {
		problems = append(problems, fmt.Sprintf("%d %s flagged incomplete", h.Incomplete, plural(h.Incomplete, "layer", "layers")))
	}
	if len(h.Warnings) > 0 {
		detail := summary + "; podman reports storage corruption: " + strings.Join(h.Warnings, " / ")
		if len(problems) > 0 {
			detail += "; " + strings.Join(problems, ", ")
		}
		return checkResult{podmanStorageCheck, false, detail, podmanResetFix}
	}
	if len(problems) == 0 {
		return checkResult{podmanStorageCheck, true, summary + ", no orphan layers", ""}
	}
	fix := "lever leaves these alone and never deletes a layer. They come from image loads that failed or were " +
		"interrupted, and a later load can reuse a damaged one and fail. "
	if h.MajorVersion() >= 5 {
		fix += "On podman 5.x, `podman system check` in the guest as the jail user lists them, and after a backup " +
			"`podman system check --repair` removes those older than a day; or, "
	} else {
		fix += "podman before 5.x has no command that removes a layer no image owns. "
	}
	fix += podmanResetFix
	return warnResult(podmanStorageCheck, summary+"; "+strings.Join(problems, ", "), fix)
}

func orUnknown(s string) string {
	if s == "" {
		return "(version unknown)"
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// humanBytes renders n as B, KB, MB or GB (powers of 1000, one decimal).
func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f, suffix := float64(n), "B"
	for _, s := range []string{"KB", "MB", "GB", "TB"} {
		if f < unit {
			break
		}
		f /= unit
		suffix = s
	}
	return fmt.Sprintf("%.1f %s", f, suffix)
}
