package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/jail"
	scionpkg "github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

// claudeSettingsReader reads back the Claude config in one agent container
// (jail.AgentProbe.ClaudeSettings in production).
type claudeSettingsReader func(ctx context.Context, ref string) (jail.ClaudeDelivered, error)

// claudeAgent is one agent the claude-settings row compares: its scion slug
// and its configured Claude config (nil = none).
type claudeAgent struct {
	name string
	want *wire.Claude
}

// checkClaudeSettings compares each running agent's managed-settings file
// (what boot wrote at its last start) with its claude_settings and
// after_compact_note. The value is applied at start, so a difference on a
// running agent is a warning: it takes the new value at its next start. A
// container that lacks the value after a start runs an image whose
// lever-agent predates the claude block. With nothing configured anywhere
// the row reads nothing.
func checkClaudeSettings(ctx context.Context, project string, agents []claudeAgent, list agentLister, read claudeSettingsReader) checkResult {
	const check = "claude settings"
	configured := false
	for _, a := range agents {
		if !a.want.IsZero() {
			configured = true
		}
	}
	if !configured {
		return checkResult{check, true, "none configured", ""}
	}
	if list == nil || read == nil {
		return checkResult{check, true, "not checked", ""}
	}
	recs, err := list(ctx, project)
	if err != nil {
		return checkResult{check, true, "not checked (could not list agents): " + firstLine(err.Error()), ""}
	}
	var good, differ, unread, idle []string
	for _, a := range agents {
		rec := scionpkg.FindAgent(recs, a.name)
		if rec == nil || !scionpkg.ContainerLive(rec.ContainerStatus) {
			if !a.want.IsZero() {
				idle = append(idle, a.name)
			}
			continue
		}
		got, err := read(ctx, jail.ContainerName(hubProjectKey(project), a.name))
		switch {
		case err != nil:
			unread = append(unread, a.name+" ("+jail.ProbeErrorClass(err)+")")
		case got.Matches(a.want):
			if !a.want.IsZero() {
				good = append(good, a.name+" ("+jail.DescribeClaude(a.want)+")")
			}
		default:
			has := got.Describe()
			if has == jail.DescribeClaude(a.want) {
				has += ", but a different note"
			}
			differ = append(differ, fmt.Sprintf("%s (configured %s; has %s)", a.name, jail.DescribeClaude(a.want), has))
		}
	}
	var tail []string
	if len(idle) > 0 {
		tail = append(tail, "not running (applied at start): "+strings.Join(idle, ", "))
	}
	if len(unread) > 0 {
		tail = append(tail, "not checked (unreadable): "+strings.Join(unread, ", "))
	}
	if len(differ) > 0 {
		detail := "differs from the config: " + strings.Join(differ, ", ")
		if len(good) > 0 {
			detail += "; as configured: " + strings.Join(good, ", ")
		}
		if len(tail) > 0 {
			detail += "; " + strings.Join(tail, "; ")
		}
		return warnResult(check, detail,
			"the settings apply at an agent's next start: `lever stop && lever up` for the manager, a stop and a resume for a worker. If they still differ after a start, the agent image predates this lever (rebuild it with `make lever-image`), or the manager rewrote its own .lever/bootstrap.json")
	}
	var parts []string
	if len(good) > 0 {
		parts = append(parts, strings.Join(good, ", "))
	}
	parts = append(parts, tail...)
	if len(parts) == 0 {
		return checkResult{check, true, "no running agent", ""}
	}
	return checkResult{check, true, strings.Join(parts, "; "), ""}
}

// claudeAgents is the manager and each worker with its configured Claude
// config, in the row's order.
func claudeAgents(manager string, managerWant *wire.Claude, workers []string, workerWant map[string]*wire.Claude) []claudeAgent {
	out := []claudeAgent{{manager, managerWant}}
	for _, w := range workers {
		out = append(out, claudeAgent{w, workerWant[w]})
	}
	return out
}
