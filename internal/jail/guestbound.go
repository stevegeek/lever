package jail

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/stevegeek/lever/internal/proc"
)

// A host-side deadline (BoundAgentExec) kills only the host process: the
// `orb` or `limactl shell` client. Nothing makes the guest-side command die
// with it: a `podman exec` whose program the agent made hang (a FIFO token
// file, a fake tmux or sciontool), or a guest read that walks a mount the
// agent controls, would stay behind in the guest, one more on every
// token-watch pass. So every agent-facing guest command also runs under the
// guest's own timeout(1), with a deadline shorter than the host's: the guest
// kills the command and its process group first (SIGTERM, then SIGKILL
// guestKillAfter later), and the host deadline stays as the backstop for a
// transport that hangs on its own. timeout(1) is the guest's coreutils,
// which lever trusts like the rest of the guest; the command it runs is
// looked up on the jail's fixed PATH.

// guestKillAfter is how long timeout(1) waits after SIGTERM before SIGKILL.
const guestKillAfter = time.Second

// guestMargin is how much earlier than the host the guest gives up: the
// SIGKILL grace plus a second for the transport to carry the exit back.
const guestMargin = guestKillAfter + time.Second

// guestTimeoutSecs is the guest-side deadline in whole seconds (at least 1)
// for a host deadline of agentExecTimeout.
func guestTimeoutSecs() int {
	s := int((agentExecTimeout - guestMargin) / time.Second)
	return max(s, 1)
}

// guestBounded is the argv that runs name args under the guest's timeout(1).
func guestBounded(name string, args ...string) (string, []string) {
	return "timeout", append([]string{
		"--kill-after=" + strconv.Itoa(int(guestKillAfter/time.Second)) + "s",
		strconv.Itoa(guestTimeoutSecs()) + "s",
		name,
	}, args...)
}

// Exit statuses timeout(1) reports for a command it ended: 124 after
// SIGTERM, 128+9 when SIGKILL was needed.
const (
	guestTimedOut = 124
	guestKilled   = 137
)

// runGuestBounded runs name args in the guest under guestBounded through r.
// A command the guest timed out is an error that wraps ErrProbeTimedOut (the
// program inside a container can exit 124 by itself too: then it is still a
// failed probe, only named as a timeout). res is returned as r gave it.
func runGuestBounded(ctx context.Context, r proc.Runner, name string, args ...string) (proc.Result, error) {
	n, a := guestBounded(name, args...)
	res, err := r.Run(ctx, nil, n, a...)
	return res, guestTimeoutErr(res, err)
}

// runGuestBoundedStdin is runGuestBounded with stdin.
func runGuestBoundedStdin(ctx context.Context, r proc.Runner, stdin io.Reader, name string, args ...string) (proc.Result, error) {
	n, a := guestBounded(name, args...)
	res, err := r.RunStdin(ctx, stdin, nil, n, a...)
	return res, guestTimeoutErr(res, err)
}

func guestTimeoutErr(res proc.Result, err error) error {
	if err != nil && (res.Code == guestTimedOut || res.Code == guestKilled) {
		return fmt.Errorf("%w (guest exit status %d)", ErrProbeTimedOut, res.Code)
	}
	return err
}
