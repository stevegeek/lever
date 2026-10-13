package apply

import (
	"context"
	"path"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
)

// GuestBoot ties a manager record to the jail guest boot it last ran in
// (lever#179). A guest that was killed hard (a wedged kernel, Lima killing
// QEMU) and restarted leaves the hub record saying "running" over a
// container that never started in the new boot. Nothing suspended the
// manager, so `lever up` found it "running", observed a dead container and
// told the operator to run `lever stop` and `lever up` (the suspend there
// is what makes the next up resume it). With the boot recorded, apply and
// up tell that case apart from a harness that died in a live guest, and do
// the suspend and resume themselves; the conversation is kept, never
// --fresh.
//
// Current reads the guest's /proc/sys/kernel/random/boot_id (new on every
// boot). Recorded and Record keep, host-side, the boot id in which lever
// last saw the manager live; Recorded returns "" when none is kept.
type GuestBoot interface {
	Current(ctx context.Context) (string, error)
	Recorded() (string, error)
	Record(id string) error
}

// guestRestartedUnder reports whether rec is a manager record left over
// from a guest boot that has ended: the record says running, lever saw the
// manager live in an earlier boot than the current one, and podman in the
// guest says the manager's container is not running now. Only podman's
// own answer counts: the hub briefly reports "created" for a container
// that is starting, so the hub's status is never evidence of a dead one,
// and a podman read that fails (or no podman probe at all) answers no. Any
// read that fails answers no, which leaves today's behaviour in place.
// With no boot id kept, podman's start time stands in for it
// (startedInEndedBoot).
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

// podmanSaysStopped reports podman's own answer that the container ref is
// not running; no probe, or a probe that fails, answers no.
func podmanSaysStopped(ctx context.Context, d Deps, ref string) bool {
	if d.ContainerRunning == nil {
		return false
	}
	running, err := d.ContainerRunning(ctx, ref)
	return err == nil && !running
}

// startedInEndedBoot is guestRestartedUnder's evidence when lever kept no
// boot id: the instance's last bring-up predates the record (an upgrade),
// or the record file was lost. Then podman must say both that the
// container is not running and that it last started before the current
// boot (jail.ContainerStartedBeforeBoot). A container that never started,
// or any read that fails, answers no.
func startedInEndedBoot(ctx context.Context, d Deps, ref string) bool {
	if d.ContainerStartedBeforeBoot == nil || !podmanSaysStopped(ctx, d, ref) {
		return false
	}
	before, err := d.ContainerStartedBeforeBoot(ctx, ref)
	return err == nil && before
}

// suspendAfterGuestRestart suspends a manager record guestRestartedUnder
// found, so the caller's resume path brings it back with its conversation
// (what `lever stop` + `lever up` did by hand). A failed suspend is logged;
// the caller re-reads the record and acts on what it finds.
func suspendAfterGuestRestart(ctx context.Context, d Deps, subject, project, name string, rec *scion.Agent) {
	d.logf("%s: manager %q: the jail VM restarted since the manager last ran (a new guest boot) and its container is %s; suspending and resuming it (the conversation is kept)",
		subject, name, scion.BoundedQuote(rec.ContainerStatus))
	if err := d.Scion.Suspend(ctx, name, project); err != nil {
		d.logf("%s: warning: suspend of manager %q failed: %s", subject, name, scion.ErrSummary(err))
	}
}

// RecoverAfterGuestRestart is `lever up`'s half of the guest-restart
// recovery, for a manager up found running: when the record is left over
// from an ended guest boot (guestRestartedUnder) it suspends it and
// reports true once the record reads suspended, so up takes its resume
// path. False otherwise, including when the suspend did not take: up then
// observes the record as before.
func RecoverAfterGuestRestart(ctx context.Context, d Deps, name, project string) bool {
	agents, err := d.Scion.List(ctx, project)
	if err != nil {
		return false
	}
	rec := scion.FindAgent(agents, name)
	if !guestRestartedUnder(ctx, d, project, name, rec) {
		return false
	}
	suspendAfterGuestRestart(ctx, d, "up", project, name, rec)
	agents, err = d.Scion.List(ctx, project)
	if err != nil {
		return false
	}
	rec = scion.FindAgent(agents, name)
	return rec != nil && rec.Phase == scion.PhaseSuspended
}

// logf is d.Log, or nothing when it is nil (the paths outside Run).
func (d Deps) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log(format, args...)
	}
}

// RecordGuestBoot notes the current guest boot as the one the manager runs
// in. Called once lever has seen the manager live. Best-effort: a failure
// only means the next restart is not recognised (logged).
func RecordGuestBoot(ctx context.Context, d Deps) {
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
