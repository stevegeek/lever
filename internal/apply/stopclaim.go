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
	"github.com/stevegeek/lever/internal/wait"
)

// StaleContainerOps is what clearing a stop claim needs from podman in the
// jail (jail.ContainerJanitor). Every answer is podman's own; none comes from
// the agent or its hub record.
type StaleContainerOps interface {
	State(ctx context.Context, ref string) (jail.ContainerState, error)
	Remove(ctx context.Context, id string) error
}

// ErrStopClaimHeld marks a resume the hub still refuses with its stop claim
// after lever's recovery, or one lever could not recover.
var ErrStopClaimHeld = errors.New("the hub holds a stop claim on the manager")

var stopClaimMaxInterval = wait.StopClaimReleased.MaxInterval

// stopClaimSettle is how long a container must keep the same id and the same
// state between two looks before it is removed. created is also the state
// between scion's create and its start, so a container must stay created far
// longer than a start takes before it counts as an interrupted one; the
// wait is measured on lever's own clock, as podman's creation time is on the
// guest's.
var stopClaimSettle = struct{ Stopped, Created time.Duration }{5 * time.Second, 30 * time.Second}

// The hub releases the claim on the reaper's next tick (every minute) after a
// broker heartbeat shows the container gone: 5+10+20+30*5 s, about 3 min.
var defaultStopClaimRetry = budgetOf(wait.StopClaimReleased)

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
// was inspected, once a second look after stopClaimSettle shows the same id in
// the same state, and without --force. Its writable layer is lost, as on any
// scion resume of a container that is not running; lever keeps no copy, since
// a committed image would hold the container's environment, credentials
// included, as a runnable image. The conversation is in the agent home, a bind
// mount, so the removal does not touch it.
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

// clearStaleContainer removes the stopped container st once a second look
// shows it unchanged. It returns an error wrapping ErrStopClaimHeld when it
// removed nothing.
func clearStaleContainer(ctx context.Context, d Deps, subject, ref string, st jail.ContainerState) error {
	ops := d.StaleContainer
	settle := stopClaimSettle.Stopped
	if st.Status != "exited" && st.Status != "stopped" {
		settle = stopClaimSettle.Created
	}
	d.Log("%s: container %s is %s; checking again in %s that it stays so before removing it", subject, ref, st.Status, settle)
	if !sleepCtx(ctx, settle) {
		return ctx.Err()
	}
	again, aerr := ops.State(ctx, ref)
	switch {
	case errors.Is(aerr, jail.ErrNoContainer):
		d.Log("%s: container %s was removed by someone else meanwhile", subject, ref)
		return nil
	case aerr != nil:
		return stopClaimFailure(subject, ref, "lever could not read the container's state again before removing it ("+termsafe.Sanitize(aerr.Error())+"), so it removed nothing", nil)
	case again.ID != st.ID || again.Status != st.Status || !again.Removable():
		return stopClaimFailure(subject, ref, "the container changed between lever's two looks (now "+termsafe.Sanitize(again.Status)+"), so lever removed nothing; a start may be under way", nil)
	}
	if rerr := ops.Remove(ctx, st.ID); rerr != nil {
		return stopClaimFailure(subject, ref, "removing the container failed ("+termsafe.Sanitize(rerr.Error())+")", nil)
	}
	d.Log("%s: removed manager container %s (podman state %s) so the hub can release its stop claim. The resume creates a new container, as any scion resume of a container that is not running does: its writable layer is lost, that is whatever was written outside the agent home and the mounted tree (packages installed by hand, files in /tmp). The conversation is in the agent home and is kept. Waiting for the hub to release the claim (at most about %s)",
		subject, ref, st.Status, stopClaimWaitTotal(d.StopClaimRetry.or(defaultStopClaimRetry)).Round(time.Second))
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
		"`podman rm %s` removes it, and with it its writable layer (files written outside the agent home and the tree, such as packages installed by hand), as any scion resume of a stopped container does. "+
		"The hub releases the claim within about two minutes of the broker's next heartbeat; then run `lever up`. "+
		"The conversation is in the agent home and is not touched",
		ref, ref)
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
