package jail

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/wire"
)

// ClaudeReady is one answer of AwaitClaudeReady.
type ClaudeReady int

const (
	// ClaudeReadyYes: claude's pane is alive and the claude-ready marker
	// names this container run and that pane.
	ClaudeReadyYes ClaudeReady = iota + 1
	// ClaudeReadyPending: the pane is alive and no matching marker appeared
	// within the window.
	ClaudeReadyPending
	// ClaudeReadyNoHook: the pane is alive, but the managed settings carry
	// no claude-ready hook (an older image, or a boot that could not write
	// the file): no marker will come.
	ClaudeReadyNoHook
	// ClaudeReadyPaneGone: tmux did not list a live pane in scion's agent
	// window. The caller reads the pane again (HarnessAlive) to tell a dead
	// claude from a tmux that did not answer.
	ClaudeReadyPaneGone
)

// ErrReadyShape is AwaitClaudeReady's answer for output it does not know.
var ErrReadyShape = errors.New("waiting for claude's start: unexpected output")

const readyStep = 200 * time.Millisecond

// readyScript polls until claude's tmux pane is alive and the marker $1
// equals wire.ReadyMarker for this container run and that pane, and prints
// ready, pending, nohook or gone. $5 is the /proc root (a test seam). It must
// build the run identity exactly as agent.RunIdentity does.
//
// Globbing stays off (set -f): the pane ids tmux prints are split, never
// expanded.
//
// Everything it reads is the agent's: the marker is compared for equality,
// never parsed; a symbolic link at the marker counts as no marker; every
// read is bounded, and a FIFO that blocks one ends at the exec's deadline.
const readyScript = `m=$1 n=$2 hook=$3 settings=$4 proc=$5
read -r boot < "$proc/sys/kernel/random/boot_id" || exit 4
st=$(head -c 1024 "$proc/1/stat") || exit 4
st=${st##*)}
set -f
set -- $st
[ $# -ge 20 ] || exit 4
eval "st=\${20}"
hooked= i=0
while :; do
  panes=$(tmux list-panes -t scion:agent -F '#{pane_dead} #{pane_id}' 2>/dev/null) || { echo gone; exit 0; }
  live=$(printf '%s\n' "$panes" | while read -r dead id; do if [ "$dead" = 0 ]; then printf '%s\n' "$id"; fi; done)
  [ -n "$live" ] || { echo gone; exit 0; }
  if [ -z "$hooked" ]; then
    head -c 65536 "$settings" 2>/dev/null | grep -qF "$hook" || { echo nohook; exit 0; }
    hooked=1
  fi
  if [ -f "$m" ] && [ ! -L "$m" ]; then
    got=$(head -c 256 "$m" 2>/dev/null) || got=
    for id in $live; do
      [ "$got" = "v1 $boot $st $id" ] && { echo ready; exit 0; }
    done
  fi
  i=$((i+1))
  [ "$i" -lt "$n" ] || { echo pending; exit 0; }
  sleep 0.2
done`

// AwaitClaudeReady waits in the container, for up to window, until claude's
// pane is alive and lever's claude-ready SessionStart hook has written the
// marker for this container run and that pane. window must leave the exec
// its margin under agentExecTimeout.
//
// The answer is the agent's word: an agent that writes the marker itself
// claims only that claude started, which a live pane already claims. Only
// one of four fixed words leaves the container.
func (p AgentProbe) AwaitClaudeReady(ctx context.Context, ref string, window time.Duration) (ClaudeReady, error) {
	if err := checkRef(ref); err != nil {
		return 0, fmt.Errorf("waiting for claude's start: %w", err)
	}
	ctx, cancel := BoundAgentExec(ctx)
	defer cancel()
	n := max(int(window/readyStep), 1)
	res, err := runGuestBounded(ctx, p.R, "podman", "exec", "--user", AgentUser, ref, "sh", "-c", readyScript, "sh",
		wire.ReadyMarkerPath, strconv.Itoa(n), wire.ReadyHookCommand, wire.ManagedSettingsPath, "/proc")
	if err != nil {
		switch {
		case noSuchContainer(res.Stderr):
			return 0, fmt.Errorf("waiting for claude's start in %s: %w", ref, ErrNoContainer)
		case errors.Is(err, proc.ErrOutputLimit):
			return 0, fmt.Errorf("waiting for claude's start in %s: %w", ref, proc.ErrOutputLimit)
		case ctx.Err() != nil, errors.Is(err, ErrProbeTimedOut):
			return 0, fmt.Errorf("waiting for claude's start in %s: %w", ref, ErrProbeTimedOut)
		}
		return 0, fmt.Errorf("waiting for claude's start in %s: %w", ref, &ProbeExitError{Code: res.Code})
	}
	switch strings.TrimSpace(res.Stdout) {
	case "ready":
		return ClaudeReadyYes, nil
	case "pending":
		return ClaudeReadyPending, nil
	case "nohook":
		return ClaudeReadyNoHook, nil
	case "gone":
		return ClaudeReadyPaneGone, nil
	}
	return 0, fmt.Errorf("waiting for claude's start in %s: %w", ref, ErrReadyShape)
}
