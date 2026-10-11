package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/jail"
)

const podmanStorageCheck = "podman storage"

// podmanResetFix is the recovery every damaged-store verdict points at: back
// up, reset the store as the jail user, bring the instance up again on the
// same records. The runbook in the operations guide has the detail.
const podmanResetFix = "if `lever up` fails at load-image: back up the guest's ~/.scion (hub database, agent homes) first, " +
	"then in the guest as the jail user run `podman system reset --force` (it removes every image, container and " +
	"podman volume in the jail; the hub records, agent homes and the tree stay), then `lever up` without --fresh; " +
	"see \"Recovering the jail's podman store\" in the operations guide"

// checkPodmanStorage reports the health of the jail's rootless podman store
// (lever#173): podman's own corruption warning fails the row; layers no image
// or container uses, or layers flagged incomplete, warn. Orphan layers are
// harmless until a load reuses a damaged one, which is how the reported
// load-image failure happened. read is jail.ReadStoreHealth over the jail
// runner: read-only, bounded, as the jail run user. lever never deletes a
// layer; the fix says how to recover.
func checkPodmanStorage(ctx context.Context, read func(context.Context) (jail.StoreHealth, error)) checkResult {
	h, err := read(ctx)
	if err != nil {
		return checkResult{podmanStorageCheck, true, "not checked (could not read the jail's podman store): " + firstLine(err.Error()), ""}
	}
	summary := fmt.Sprintf("podman %s, %s: %d %s, %d %s, %d %s", orUnknown(h.Version), h.Driver,
		h.Images, plural(h.Images, "image", "images"), h.Containers, plural(h.Containers, "container", "containers"),
		h.Layers, plural(h.Layers, "layer", "layers"))
	var problems []string
	if h.Orphans > 0 {
		problems = append(problems, fmt.Sprintf("%d %s no image or container uses (%s)", h.Orphans, plural(h.Orphans, "layer", "layers"), humanBytes(h.OrphanBytes)))
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
