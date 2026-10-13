package jail

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/stevegeek/lever/internal/proc"
)

// A host-side deadline (BoundAgentExec) kills only the host `orb` or
// `limactl shell` client; a guest command the agent made hang would stay
// behind in the guest, one more on every token-watch pass. So every
// agent-facing guest command also runs under the guest's own timeout(1),
// with a shorter deadline, and the host deadline stays as the backstop for a
// transport that hangs on its own. timeout(1) is the guest's coreutils,
// which lever trusts like the rest of the guest.

const guestKillAfter = time.Second

// guestMargin is how much earlier than the host the guest gives up: the
// SIGKILL grace plus a second for the transport to carry the exit back.
const guestMargin = guestKillAfter + time.Second

// guestTimeoutSecs is what is left of ctx's deadline, never a fresh full
// span: a caller runs several guest commands under one BoundAgentExec
// deadline.
func guestTimeoutSecs(ctx context.Context) int {
	left := agentExecTimeout
	if d, ok := ctx.Deadline(); ok {
		left = time.Until(d)
	}
	// Rounded up, which eats less than the second of transport slack in
	// guestMargin, so the guest still ends first.
	return max(int((left-guestMargin+time.Second-1)/time.Second), 1)
}

func guestBounded(ctx context.Context, name string, args ...string) (string, []string) {
	return "timeout", append([]string{
		"--kill-after=" + strconv.Itoa(int(guestKillAfter/time.Second)) + "s",
		strconv.Itoa(guestTimeoutSecs(ctx)) + "s",
		name,
	}, args...)
}

// Exit statuses timeout(1) reports for a command it ended: 124 after
// SIGTERM, 128+9 when SIGKILL was needed.
const (
	guestTimedOut = 124
	guestKilled   = 137
)

// runGuestBounded wraps ErrProbeTimedOut for a command the guest timed out.
// A program in a container can exit 124 by itself too; it is still a failed
// probe, only named as a timeout.
func runGuestBounded(ctx context.Context, r proc.Runner, name string, args ...string) (proc.Result, error) {
	n, a := guestBounded(ctx, name, args...)
	res, err := r.Run(ctx, nil, n, a...)
	return res, guestTimeoutErr(res, err)
}

func runGuestBoundedStdin(ctx context.Context, r proc.Runner, stdin io.Reader, name string, args ...string) (proc.Result, error) {
	n, a := guestBounded(ctx, name, args...)
	res, err := r.RunStdin(ctx, stdin, nil, n, a...)
	return res, guestTimeoutErr(res, err)
}

func guestTimeoutErr(res proc.Result, err error) error {
	if err != nil && (res.Code == guestTimedOut || res.Code == guestKilled) {
		return fmt.Errorf("%w (guest exit status %d)", ErrProbeTimedOut, res.Code)
	}
	return err
}
