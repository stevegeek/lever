package apply

import (
	"context"
	"path"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
)

// GuestBoot ties a manager record to the jail guest boot it last ran in. A
// guest killed hard and restarted leaves the hub record saying "running" over
// a container that never started in the new boot; with the boot recorded,
// apply and up tell that apart from a harness that died in a live guest and
// suspend and resume the manager themselves, keeping the conversation.
//
// Current reads the guest's boot_id (new on every boot). Recorded and Record
// keep, host-side, the boot id in which lever last saw the manager live;
// Recorded returns "" when none is kept.
type GuestBoot interface {
	Current(ctx context.Context) (string, error)
	Recorded() (string, error)
	Record(id string) error
}

// guestRestartedUnder reports whether rec is a running manager record left
// over from an ended guest boot. Only podman's answer counts as evidence the
// container is dead: the hub briefly reports "created" for a container that
// is starting. Any read that fails answers no. With no boot id kept, podman's
// start time stands in for it (startedInEndedBoot).
func guestRestartedUnder(ctx context.Context, d Deps, project, name string, rec *scion.Agent) bool {
	if rec == nil || rec.Phase != scion.PhaseRunning || d.GuestBoot == nil {
		return false
	}
	was, err := d.GuestBoot.Recorded()
	if err != nil {
		return false
	}
	ref := jail.ContainerName(path.Base(project), name)
	if was == "" {
		return startedInEndedBoot(ctx, d, ref)
	}
	now, err := d.GuestBoot.Current(ctx)
	if err != nil || now == "" || now == was {
		return false
	}
	return podmanSaysStopped(ctx, d, ref)
}

// podmanSaysStopped answers no when there is no probe or the probe fails.
func podmanSaysStopped(ctx context.Context, d Deps, ref string) bool {
	if d.ContainerRunning == nil {
		return false
	}
	running, err := d.ContainerRunning(ctx, ref)
	return err == nil && !running
}

// startedInEndedBoot is guestRestartedUnder's evidence when no boot id is
// kept: podman must say both that the container is not running and that it
// last started before the current boot. A container that never started, or
// any read that fails, answers no.
func startedInEndedBoot(ctx context.Context, d Deps, ref string) bool {
	if d.ContainerStartedBeforeBoot == nil || !podmanSaysStopped(ctx, d, ref) {
		return false
	}
	before, err := d.ContainerStartedBeforeBoot(ctx, ref)
	return err == nil && before
}

// suspendAfterGuestRestart suspends so the caller's resume path brings the
// manager back with its conversation. A failed suspend is only logged: the
// caller re-reads the record and acts on what it finds.
func suspendAfterGuestRestart(ctx context.Context, d Deps, subject, project, name string, rec *scion.Agent) {
	d.logf("%s: manager %q: the jail VM restarted since the manager last ran (a new guest boot) and its container is %s; suspending and resuming it (the conversation is kept)",
		subject, name, scion.BoundedQuote(rec.ContainerStatus))
	if err := d.managerScion().Suspend(ctx, name, project); err != nil {
		d.logf("%s: warning: suspend of manager %q failed: %s", subject, name, scion.ErrSummary(err))
	}
}

func (d Deps) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log(format, args...)
	}
}

// recordGuestBoot notes the current guest boot as the one the manager runs
// in; call it once lever has seen the manager live. Best-effort: a failure
// only means the next restart is not recognised.
func recordGuestBoot(ctx context.Context, d Deps) {
	if d.GuestBoot == nil {
		return
	}
	id, err := d.GuestBoot.Current(ctx)
	if err != nil || id == "" {
		return
	}
	if was, err := d.GuestBoot.Recorded(); err == nil && was == id {
		return
	}
	if err := d.GuestBoot.Record(id); err != nil {
		d.logf("start-manager: warning: could not record the guest boot id (a VM restart will not be recognised): %v", err)
	}
}
