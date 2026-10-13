package jail

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/stevegeek/lever/internal/proc"
)

// logTailLines is enough to hold the harness's last words through a terminal
// redraw. logTailOutputLimit is above the general agent-exec cap because
// terminal lines carry control sequences.
const (
	logTailLines       = 200
	logTailOutputLimit = 256 << 10
)

// LogTail returns the last lines of the agent container's log, stdout and
// stderr together. It works on an exited container too.
//
// The log is the agent's word: a caller only matches fixed phrases in it and
// never prints or forwards it; no error repeats it. At the output cap what
// was read is still returned with proc.ErrOutputLimit; it holds the older
// lines, so a phrase may be missed, never invented.
func (p AgentProbe) LogTail(ctx context.Context, ref string) (string, error) {
	if err := checkRef(ref); err != nil {
		return "", fmt.Errorf("reading agent container log: %w", err)
	}
	ctx, cancel := BoundAgentExec(ctx)
	defer cancel()
	ctx = proc.WithOutputLimit(ctx, logTailOutputLimit)
	res, err := runGuestBounded(ctx, p.R, "podman", "logs", "--tail", strconv.Itoa(logTailLines), ref)
	out := res.Stdout + "\n" + res.Stderr
	if err != nil {
		switch {
		case noSuchContainer(res.Stderr):
			return "", fmt.Errorf("reading agent container log in %s: %w", ref, ErrNoContainer)
		case errors.Is(err, proc.ErrOutputLimit):
			return out, fmt.Errorf("reading agent container log in %s: %w", ref, proc.ErrOutputLimit)
		case ctx.Err() != nil, errors.Is(err, ErrProbeTimedOut):
			return "", fmt.Errorf("reading agent container log in %s: %w", ref, ErrProbeTimedOut)
		}
		return "", fmt.Errorf("reading agent container log in %s: %w", ref, &ProbeExitError{Code: res.Code})
	}
	return out, nil
}
