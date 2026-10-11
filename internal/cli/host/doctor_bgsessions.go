package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/jail"
	scionpkg "github.com/stevegeek/lever/internal/scion"
)

// bgSessionsReader counts stale Claude background-session records in one
// agent container (jail.AgentProbe.BackgroundSessions in production).
type bgSessionsReader func(ctx context.Context, ref string) (jail.BackgroundRecords, error)

// checkBackgroundSessions looks in each running agent's home for Claude
// session records of a background session ("kind":"bg", or an interactive
// record with a "parkedJobId") that another container registered. Claude
// cannot tell whether such a session still runs, so the next claude
// --continue (scion's restart) refuses and the harness dies at start. lever
// turns background sessions off at every start; a record left from before,
// or from an image whose lever-agent predates that, still blocks. A warning:
// the running agent is fine until it restarts. Only counts are reported.
func checkBackgroundSessions(ctx context.Context, project string, agents []string, list agentLister, read bgSessionsReader) checkResult {
	const check = "claude background sessions"
	if list == nil || read == nil {
		return checkResult{check, true, "not checked", ""}
	}
	recs, err := list(ctx, project)
	if err != nil {
		return checkResult{check, true, "not checked (could not list agents): " + firstLine(err.Error()), ""}
	}
	var stale, unread, notes []string
	checked := 0
	for _, name := range agents {
		rec := scionpkg.FindAgent(recs, name)
		if rec == nil || !scionpkg.ContainerLive(rec.ContainerStatus) {
			continue
		}
		got, err := read(ctx, jail.ContainerName(hubProjectKey(project), name))
		if err != nil {
			unread = append(unread, name+" ("+jail.ProbeErrorClass(err)+")")
			continue
		}
		checked++
		if got.Stale > 0 {
			stale = append(stale, fmt.Sprintf("%s (%d)", name, got.Stale))
		}
		if got.Unreadable > 0 {
			notes = append(notes, fmt.Sprintf("%s: %d unreadable record(s)", name, got.Unreadable))
		}
		if got.More {
			notes = append(notes, name+": more records than doctor reads")
		}
	}
	var tail []string
	if len(unread) > 0 {
		tail = append(tail, "not checked (unreadable): "+strings.Join(unread, ", "))
	}
	tail = append(tail, notes...)
	if len(stale) > 0 {
		detail := "stale background-session records that will block the next claude --continue (the agent dies at its next start): " + strings.Join(stale, ", ")
		if len(tail) > 0 {
			detail += "; " + strings.Join(tail, "; ")
		}
		return warnResult(check, detail, bgSessionsFix)
	}
	var parts []string
	if checked > 0 {
		parts = append(parts, "none stale")
	}
	parts = append(parts, tail...)
	if len(parts) == 0 {
		return checkResult{check, true, "no running agent", ""}
	}
	return checkResult{check, true, strings.Join(parts, "; "), ""}
}

// bgSessionsFix is the background-sessions row's fix.
const bgSessionsFix = "in the agent's container (its home is /home/scion), move aside the ~/.claude/sessions/*.json records that hold " +
	"\"kind\":\"bg\" or a \"parkedJobId\", and ~/.claude/jobs/ and ~/.claude/daemon*; the conversation transcript stays. " +
	"Do not use /background, /bg or the left-arrow key in an agent tmux. See the operations guide, \"Background sessions and /login\""
