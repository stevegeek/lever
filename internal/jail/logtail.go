package jail

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/stevegeek/lever/internal/proc"
)

// logTailLines is how many lines of an agent container's log LogTail reads:
// enough to hold the harness's last words, and small enough to stay under the
// exec cap (a cut read keeps the older lines and loses the newest).
const logTailLines = 50

// LogTail returns the last lines of the agent container's log (podman logs
// --tail, run in the guest), stdout and stderr together. It works on an
// exited container too, which is when a caller wants it: the harness died.
//
// The log is the agent's word and may carry anything the agent printed. A
// caller matches fixed phrases in it and never prints or forwards it; no
// error repeats it. The read is bounded like every agent exec
// (BoundAgentExec): podman is stopped at the cap, and what was read up to
// it is still returned with proc.ErrOutputLimit (it holds the older lines of
// the tail, so a phrase may be missed, never invented).
func (p AgentProbe) LogTail(ctx context.Context, ref string) (string, error) {
	if err := checkRef(ref); err != nil {
		return "", fmt.Errorf("reading agent container log: %w", err)
	}
	ctx, cancel := BoundAgentExec(ctx)
	defer cancel()
	res, err := p.R.Run(ctx, nil, "podman", "logs", "--tail", strconv.Itoa(logTailLines), ref)
	out := res.Stdout + "\n" + res.Stderr
	if err != nil {
		switch {
		case noSuchContainer(res.Stderr):
			return "", fmt.Errorf("reading agent container log in %s: %w", ref, ErrNoContainer)
		case errors.Is(err, proc.ErrOutputLimit):
			return out, fmt.Errorf("reading agent container log in %s: %w", ref, proc.ErrOutputLimit)
		case ctx.Err() != nil:
			return "", fmt.Errorf("reading agent container log in %s: %w", ref, ErrProbeTimedOut)
		}
		return "", fmt.Errorf("reading agent container log in %s: %w", ref, &ProbeExitError{Code: res.Code})
	}
	return out, nil
}
