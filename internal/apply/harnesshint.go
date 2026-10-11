package apply

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
)

// staleBackgroundPhrases are what Claude Code prints when claude --continue
// refuses because the last conversation moved into a background session
// (Claude Code 2.1.281). The first is the case after a container restart:
// the session was registered in another container, so this one cannot tell
// whether it still runs.
var staleBackgroundPhrases = []string{
	"moved to a background session",
	"most recent conversation is running in the background",
}

// StaleBackgroundHint is the cause and the recovery lever prints when a
// manager's harness died because of a stale background session.
const StaleBackgroundHint = "cause: Claude refused to continue, because the last conversation was moved into a " +
	"background session (/background, /bg or the left-arrow key) and its session records in the agent home " +
	"name another container. lever turns background sessions off at every start, but records left from " +
	"before still block claude --continue. Recovery: in the agent home (/home/scion in the manager's " +
	"container), move aside ~/.claude/sessions/*.json records that hold \"kind\":\"bg\" or a \"parkedJobId\", " +
	"and ~/.claude/jobs/ and ~/.claude/daemon*; the conversation transcript is kept. Then lever stop && " +
	"lever up. `lever doctor` lists such records while the container runs; see the operations guide, " +
	"\"Background sessions and /login\""

// HarnessDeathHint classifies the log tail of an agent container whose
// harness died and returns the hint to print, or "" when it knows no cause.
// The tail is the agent's word: only fixed phrases are matched, and nothing
// of it is returned.
func HarnessDeathHint(logTail string) string {
	for _, p := range staleBackgroundPhrases {
		if strings.Contains(logTail, p) {
			return StaleBackgroundHint
		}
	}
	return ""
}

// withHarnessDeathHint adds a known cause to a manager liveness failure: when
// the gate saw the harness die (or never come up), it reads the container
// log tail (d.HarnessLogTail) and appends HarnessDeathHint's text. Any other
// error, a nil reader, a failed read or an unknown cause returns err as it
// is. err stays wrapped, so errors.Is still finds the gate's sentinel.
func withHarnessDeathHint(ctx context.Context, d Deps, err error, project, name string) error {
	if err == nil || d.HarnessLogTail == nil {
		return err
	}
	if !errors.Is(err, scion.ErrAgentDied) && !errors.Is(err, scion.ErrAgentNotLive) {
		return err
	}
	tail, rerr := d.HarnessLogTail(ctx, jail.ContainerName(path.Base(project), name))
	if rerr != nil && tail == "" {
		return err
	}
	hint := HarnessDeathHint(tail)
	if hint == "" {
		return err
	}
	return fmt.Errorf("%w — %s", err, hint)
}
