package apply

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/termsafe"
)

// StaleContainerOps is what clearing a stop claim needs from podman in the
// jail (jail.ContainerJanitor). Every answer is podman's own; none comes from
// the agent or its hub record.
type StaleContainerOps interface {
	State(ctx context.Context, ref string) (jail.ContainerState, error)
	WritableSize(ctx context.Context, id string) (int64, error)
	Commit(ctx context.Context, id, image string) error
	Remove(ctx context.Context, id string) error
	PruneBackups(ctx context.Context, repo string, keep int) ([]string, error)
}

// ErrStopClaimHeld marks a resume the hub still refuses with its stop claim
// after lever's recovery, or one lever could not recover.
var ErrStopClaimHeld = errors.New("the hub holds a stop claim on the manager")

const (
	// stopClaimBackupKeep is how many backup images of one container are kept.
	stopClaimBackupKeep = 2
	// stopClaimBackupMax is the largest writable layer lever commits before it
	// removes a container. scion's own resume removes a stopped container
	// without a copy, so the layer is not something a resume keeps; a larger
	// copy on the guest's grow-only disk costs more than it saves.
	stopClaimBackupMax   int64 = 2 << 30
	stopClaimMaxInterval       = 30 * time.Second
)

// The hub releases the claim on the reaper's next tick (every minute) after a
// broker heartbeat shows the container gone: 5+10+20+30*5 s, about 3 min.
var defaultStopClaimRetry = RetryBudget{Attempts: 9, Interval: 5 * time.Second}

// ResumeClearingStopClaim runs resume and, when the hub refuses it with a
// held stop claim (IsStopClaimHeld), recovers and retries within a
// bound. The hub reads a container podman lists as created as still running
// (a start the VM's power-off interrupted leaves one), so the claim it took
// to stop that start is never released while the container exists.
//
// The refusal is matched in scion's output, which can carry text the agent
// chose; that only decides whether podman is asked. What is removed is
// decided by lever alone: the container name comes from the project and the
// configured manager name, and the state is podman's. Only a container with
// no process in it (jail.ContainerState.Removable) is removed, by the id that
// was inspected, after a second look, and without --force. It is committed to
// a backup image first; a failed commit removes nothing. The conversation is
// in the agent home, a bind mount, so neither the commit nor the removal
// touches it.
//
// It returns resume's own error for any other failure, nil once a resume
// succeeds, and an error wrapping ErrStopClaimHeld otherwise.
func ResumeClearingStopClaim(ctx context.Context, d Deps, name, project, subject string, resume func() error) error {
	err := resume()
	if !IsStopClaimHeld(err) {
		return err
	}
	ref := jail.ContainerName(path.Base(project), name)
	d.Log("%s: the hub refused to resume manager %q: it holds a stop claim that it releases only once the manager's container %s is gone or exited", subject, name, ref)
	if d.StaleContainer == nil {
		return stopClaimFailure(subject, ref, "lever cannot inspect the container from here, so it removed nothing", err)
	}
	b := d.StopClaimRetry.or(defaultStopClaimRetry)
	interval := b.Interval
	cleared := false
	lastState := "not read"
	// A removal restarts the wait: the hub needs a heartbeat and a sweep after
	// it, however late in the wait it came.
	limit := b.Attempts
	for attempt := 1; ; attempt++ {
		if !cleared {
			st, serr := d.StaleContainer.State(ctx, ref)
			switch {
			case errors.Is(serr, jail.ErrNoContainer):
				cleared, lastState = true, "gone"
			case serr != nil:
				return stopClaimFailure(subject, ref, "lever could not read the container's state from podman ("+termsafe.Sanitize(serr.Error())+"), so it removed nothing", err)
			case st.Removable():
				if cerr := clearStaleContainer(ctx, d, subject, ref, st); cerr != nil {
					return cerr
				}
				cleared, lastState = true, "removed"
				limit, interval = attempt+b.Attempts-1, b.Interval
			default:
				// The hub's own reaper stops a running container that holds the claim.
				lastState = st.Status
			}
		}
		if attempt >= limit {
			break
		}
		if !sleepCtx(ctx, interval) {
			return ctx.Err()
		}
		interval = min(interval*2, stopClaimMaxInterval)
		err = resume()
		if !IsStopClaimHeld(err) {
			return err
		}
	}
	var why string
	switch lastState {
	case "removed", "gone":
		why = fmt.Sprintf("the container is gone, but the hub still held the claim after about %s: the broker may be offline (the claim is kept while it is), or its heartbeat late. Run `lever up` again once `lever doctor` shows the broker online", stopClaimWaitTotal(b).Round(time.Second))
	default:
		why = fmt.Sprintf("podman reports the container %s, and lever never removes a container that may run a process; the hub stops it, and the next `lever up` removes it once it has stopped", strings.ToLower(termsafe.Sanitize(lastState)))
	}
	return stopClaimFailure(subject, ref, why, err)
}

// clearStaleContainer backs up and removes the stopped container st. It
// returns an error wrapping ErrStopClaimHeld when it removed nothing.
func clearStaleContainer(ctx context.Context, d Deps, subject, ref string, st jail.ContainerState) error {
	ops := d.StaleContainer
	repo, err := jail.BackupImageRepo(ref)
	if err != nil {
		return stopClaimFailure(subject, ref, "lever cannot name a backup image for it ("+err.Error()+"), so it removed nothing", nil)
	}
	image := ""
	// An unknown size is skipped like a large one: the agent can make the
	// measurement slow, and the commit could then fill the guest's disk.
	size, serr := ops.WritableSize(ctx, st.ID)
	switch {
	case serr != nil:
		d.Log("%s: lever could not measure container %s's writable layer (%s): it is removed without a backup (scion's own resume removes a stopped container without one too)",
			subject, ref, termsafe.Sanitize(serr.Error()))
	case size > stopClaimBackupMax:
		d.Log("%s: container %s's writable layer is %s, over lever's %s backup limit: it is removed without a backup (scion's own resume removes a stopped container without one too)",
			subject, ref, humanBytes(size), humanBytes(stopClaimBackupMax))
	default:
		image = repo + ":" + jail.BackupTag(nowFunc(d)())
		if cerr := ops.Commit(ctx, st.ID, image); cerr != nil {
			return stopClaimFailure(subject, ref, "backing up the container failed ("+termsafe.Sanitize(cerr.Error())+"), so lever removed nothing", nil)
		}
		d.Log("%s: backed up container %s (podman state %s, %s) as image %s; like the container, the image's configuration holds the container's environment, credentials included", subject, ref, st.Status, humanBytes(size), image)
	}
	again, aerr := ops.State(ctx, ref)
	switch {
	case errors.Is(aerr, jail.ErrNoContainer):
		d.Log("%s: container %s was removed by someone else meanwhile", subject, ref)
		return nil
	case aerr != nil:
		return stopClaimFailure(subject, ref, "lever could not read the container's state again before removing it ("+termsafe.Sanitize(aerr.Error())+"), so it removed nothing", nil)
	case again.ID != st.ID || !again.Removable():
		return stopClaimFailure(subject, ref, "the container changed while lever backed it up (now "+termsafe.Sanitize(again.Status)+"), so lever removed nothing", nil)
	}
	if rerr := ops.Remove(ctx, st.ID); rerr != nil {
		return stopClaimFailure(subject, ref, "removing the container failed ("+termsafe.Sanitize(rerr.Error())+")", nil)
	}
	kept := "no backup was taken"
	if image != "" {
		kept = "the old layer is in image " + image + " (`podman rmi " + image + "` in the guest frees its space)"
	}
	d.Log("%s: removed manager container %s (podman state %s) so the hub can release its stop claim. The resume creates a new container: whatever was written outside the agent home and the mounted tree (packages installed by hand, files in /tmp) is gone; %s. The conversation is in the agent home and is kept. Waiting for the hub to release the claim (at most about %s)",
		subject, ref, st.Status, kept, stopClaimWaitTotal(d.StopClaimRetry.or(defaultStopClaimRetry)).Round(time.Second))
	if image != "" {
		removed, perr := ops.PruneBackups(ctx, repo, stopClaimBackupKeep)
		if len(removed) > 0 {
			d.Log("%s: removed older backup images (lever keeps the newest %d): %s", subject, stopClaimBackupKeep, strings.Join(removed, ", "))
		}
		if perr != nil {
			d.Log("%s: warning: %s", subject, termsafe.Sanitize(perr.Error()))
		}
	}
	return nil
}

// stopClaimFailure is the refusal when the claim is still held. cause, when
// set, is the hub's last answer; it comes last, as in ResumeFailed.
func stopClaimFailure(subject, ref, why string, cause error) error {
	tail := ""
	if cause != nil {
		tail = " Cause: " + termsafe.Sanitize(scion.ErrSummary(cause))
	}
	return cli.WithExitCode(fmt.Errorf("%s: %w: %s. lever did NOT delete the manager: its record and its conversation are kept. "+
		"%s.%s", subject, ErrStopClaimHeld, why, StopClaimManualFix(ref), tail),
		cli.ExitManagerResumeFailed)
}

// StopClaimManualFix is the fix by hand for a stop claim a stopped container
// holds.
func StopClaimManualFix(ref string) string {
	return fmt.Sprintf("By hand, in the guest as the jail user: `podman ps -a --filter name=^%s$` must not show it Up; "+
		"`podman commit %s lever.local/%s-backup:manual` keeps a copy of its writable layer (optional; it takes as much disk as the layer, and the image holds the container's environment, credentials included); "+
		"`podman rm %s` removes it. The hub releases the claim within about two minutes of the broker's next heartbeat; then run `lever up`. "+
		"The conversation is in the agent home and is not touched",
		ref, ref, ref, ref)
}

// ResumeError maps a failed manager resume to the error its caller reports:
// the stop-claim refusal as it is, a hub refusal, or ResumeFailed.
func ResumeError(verb string, err error) error {
	switch {
	case errors.Is(err, ErrStopClaimHeld):
		return err
	case scion.IsRefusedByHub(err):
		return HubRefusedResume(verb, err)
	}
	return ResumeFailed(verb, err)
}

func stopClaimWaitTotal(b RetryBudget) time.Duration {
	var total time.Duration
	iv := b.Interval
	for i := 1; i < b.Attempts; i++ {
		total += iv
		iv = min(iv*2, stopClaimMaxInterval)
	}
	return total
}

func nowFunc(d Deps) func() time.Time {
	if d.clock != nil {
		return d.clock
	}
	return time.Now
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func humanBytes(n int64) string {
	const gib, mib = 1 << 30, 1 << 20
	if n >= gib {
		return fmt.Sprintf("%.1f GiB", float64(n)/gib)
	}
	return fmt.Sprintf("%.0f MiB", float64(n)/mib)
}

// IsStopClaimHeld reports whether err from a scion start or resume is the
// hub's 409 start_in_progress for a held stop claim (scion
// pkg/hub/start_claim.go writeStartInProgress). Such a claim is released only
// once a broker inventory shows the agent's container gone or terminal; the
// hub reads a container podman lists as Created as still running, so a
// Created container left by an interrupted start holds it for good.
func IsStopClaimHeld(err error) bool {
	if err == nil {
		return false
	}
	for line := range strings.Lines(err.Error()) {
		l := strings.ToLower(line)
		if strings.Contains(l, "via hub: start_in_progress: a stop is in progress for this agent (since ") &&
			strings.Contains(l, "(status: 409)") {
			return true
		}
	}
	return false
}
