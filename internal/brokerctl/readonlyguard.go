package brokerctl

import (
	"context"
	"fmt"
	"path"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
)

// readOnlyGuard is the production ReadOnlyGuard: it reads the manager
// container's mounts through the jail runner and requires the whole
// manager.read_only plan (entries read-only, pins in place — a missing pin
// lets the agent move an entry away), then probes the running container
// that no entry is writable (a directory replaced on the host keeps a
// listed mount over the old one). The same checks as apply's keep/resume
// warning and doctor's read-only row (apply.ManagerTreeMountGaps,
// apply.ProbeReplacedEntries). Anything it cannot read or probe — no
// container yet, a stopped one — is an error, so the tool waits.
func readOnlyGuard(app *config.App, jr proc.Runner, jailMount string) ReadOnlyGuard {
	ref := jail.ContainerName(path.Base(jailMount), app.Name)
	want := app.ManagerTreeMounts()
	probe := apply.WritableProbe(func(ctx context.Context, ref, target string) (bool, error) {
		return jail.ContainerPathWritable(ctx, jr, ref, target)
	})
	return func(ctx context.Context, _ []string) error {
		return checkManagerReadOnly(ctx, app.Name, jailMount, ref, want,
			func(ctx context.Context) ([]jail.Mount, error) { return jail.ContainerMounts(ctx, jr, ref) }, probe)
	}
}

// checkManagerReadOnly is readOnlyGuard's decision, with the mount read and
// the write probe injected for tests.
func checkManagerReadOnly(ctx context.Context, name, jailMount, ref string, want []config.TreeMount,
	mounts func(context.Context) ([]jail.Mount, error), probe apply.WritableProbe) error {
	got, err := mounts(ctx)
	if err != nil {
		return fmt.Errorf("lever cannot read manager %q's mounts (%v)", name, err)
	}
	gaps := apply.ManagerTreeMountGaps(jailMount, want, got)
	if gaps.Empty() {
		replaced, err := apply.ProbeReplacedEntries(ctx, probe, ref, want)
		if err != nil {
			return fmt.Errorf("lever cannot probe whether manager %q can write its read-only paths (%v; it may not be running yet)", name, err)
		}
		gaps.Replaced = replaced
	}
	if !gaps.Empty() {
		return fmt.Errorf("manager %q does not hold that protection: %s", name, gaps)
	}
	return nil
}
