package jail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

// AgentSessionsDir is where Claude Code keeps its live-session records in an
// agent container: one <pid>.json per running claude, interactive or in the
// background (Claude Code 2.1.296).
const AgentSessionsDir = "/home/scion/.claude/sessions"

// maxSessionRecords and sessionRecordBytes bound what the probe reads: a
// record is a few hundred bytes, and the total stays under the exec cap.
const (
	maxSessionRecords  = 24
	sessionRecordBytes = 2048
)

// bgSessionsScript prints the container's pid domain as Claude Code computes
// it ("linux:" + /etc/machine-id + ":" + the pid namespace link; a podman
// exec joins the container's pid namespace), then each regular file in the
// sessions directory named by $1 as one line after a "@@file" line, bounded
// and with its newlines folded, and "@@more" when there are more files than
// it reads. No directory prints the domain alone.
var bgSessionsScript = `printf 'linux:%s:%s\n' "$(head -c 256 /etc/machine-id 2>/dev/null)" "$(readlink /proc/self/ns/pid 2>/dev/null)"
[ -d "$1" ] || exit 0
n=0
for f in "$1"/*.json; do
  [ -f "$f" ] && [ ! -L "$f" ] || continue
  n=$((n+1))
  if [ "$n" -gt ` + strconv.Itoa(maxSessionRecords) + ` ]; then echo @@more; break; fi
  echo @@file
  head -c ` + strconv.Itoa(sessionRecordBytes) + ` "$f" | tr '\n' ' '
  echo
done`

// BackgroundRecords is what lever reads of an agent's Claude session
// records. Only counts leave the probe.
type BackgroundRecords struct {
	// Stale counts background records ("kind":"bg", or an interactive one
	// with a "parkedJobId") whose pidDomain is not this container's, or that
	// name none: Claude cannot tell whether such a session still runs, so it
	// refuses the next claude --continue.
	Stale int
	// Unreadable counts records that did not parse (or were cut at the
	// read bound).
	Unreadable int
	// More is set when the directory held more records than were read.
	More bool
}

// ErrSessionsShape is probe output with no pid-domain line.
var ErrSessionsShape = errors.New("reading agent session records: unexpected output")

// BackgroundSessions reads the agent's Claude session records in its
// container and counts stale background ones (BackgroundRecords). The
// records are the agent's word; nothing of them comes back but counts.
// ErrNoContainer when podman knows no such container.
func (p AgentProbe) BackgroundSessions(ctx context.Context, ref string) (BackgroundRecords, error) {
	if err := checkRef(ref); err != nil {
		return BackgroundRecords{}, fmt.Errorf("reading agent session records: %w", err)
	}
	ctx, cancel := BoundAgentExec(ctx)
	defer cancel()
	res, err := runGuestBounded(ctx, p.R, "podman", "exec", "--user", AgentUser, ref, "sh", "-c", bgSessionsScript, "sh", AgentSessionsDir)
	if err != nil {
		switch {
		case noSuchContainer(res.Stderr):
			return BackgroundRecords{}, fmt.Errorf("reading agent session records in %s: %w", ref, ErrNoContainer)
		case errors.Is(err, proc.ErrOutputLimit):
			return BackgroundRecords{}, fmt.Errorf("reading agent session records in %s: %w", ref, proc.ErrOutputLimit)
		case ctx.Err() != nil, errors.Is(err, ErrProbeTimedOut):
			return BackgroundRecords{}, fmt.Errorf("reading agent session records in %s: %w", ref, ErrProbeTimedOut)
		}
		return BackgroundRecords{}, fmt.Errorf("reading agent session records in %s: %w", ref, &ProbeExitError{Code: res.Code})
	}
	return parseBackgroundRecords(res.Stdout)
}

// parseBackgroundRecords reads bgSessionsScript's output.
func parseBackgroundRecords(out string) (BackgroundRecords, error) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	domain := strings.TrimSpace(lines[0])
	if !strings.HasPrefix(domain, "linux:") {
		return BackgroundRecords{}, ErrSessionsShape
	}
	var r BackgroundRecords
	for i := 1; i < len(lines); i++ {
		switch lines[i] {
		case "@@more":
			r.More = true
			continue
		case "@@file":
		default:
			continue
		}
		if i+1 >= len(lines) {
			r.Unreadable++
			break
		}
		i++
		var rec struct {
			Kind        string          `json:"kind"`
			ParkedJobID json.RawMessage `json:"parkedJobId"`
			PidDomain   *string         `json:"pidDomain"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &rec); err != nil {
			r.Unreadable++
			continue
		}
		parked := len(rec.ParkedJobID) > 0 && string(rec.ParkedJobID) != "null"
		if rec.Kind != "bg" && !parked {
			continue
		}
		if rec.PidDomain == nil || *rec.PidDomain != domain {
			r.Stale++
		}
	}
	return r, nil
}
