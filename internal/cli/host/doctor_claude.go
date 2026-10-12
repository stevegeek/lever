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
// after_compact_note. The values apply at start, so a difference on a
// running agent is a warning: it takes the config's values at its next
// start. That covers a removed config too: a running agent keeps the values
// lever wrote until it starts again, so the row probes every running agent
// even when nothing is configured. A container that still differs after a
// start runs an image whose lever-agent predates the claude block.
//
// The row also reports the agent-view switch boot writes into every agent's
// file: "agent view: off" when every running agent read has it, else "agent
// view: not set" with the agents, a warning, since such an agent can move a
// session into the background (an image without a writable /etc/claude-code
// cannot take the file).
func checkClaudeSettings(ctx context.Context, project string, agents []claudeAgent, list agentLister, read claudeSettingsReader) checkResult {
	const check = "claude settings"
	configured := false
	for _, a := range agents {
		if !a.want.IsZero() {
			configured = true
		}
	}
	if list == nil || read == nil {
		if !configured {
			return checkResult{check, true, "none configured", ""}
		}
		return checkResult{check, true, "not checked", ""}
	}
	recs, err := list(ctx, project)
	if err != nil {
		return checkResult{check, true, "not checked (could not list agents): " + firstLine(err.Error()), ""}
	}
	var good, differ, unread, idle, viewUnset []string
	viewRead := 0
	for _, a := range agents {
		rec := scionpkg.FindAgent(recs, a.name)
		if rec == nil || !scionpkg.ContainerLive(rec.ContainerStatus) {
			if !a.want.IsZero() {
				idle = append(idle, a.name)
			}
			continue
		}
		got, err := read(ctx, jail.ContainerName(hubProjectKey(project), a.name))
		if err == nil {
			viewRead++
			if !got.AgentViewOff {
				viewUnset = append(viewUnset, a.name)
			}
		}
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
	var view string
	switch {
	case len(viewUnset) > 0:
		view = "agent view: not set (background sessions possible): " + strings.Join(viewUnset, ", ")
	case viewRead > 0:
		view = "agent view: off"
	}
	if len(differ) > 0 {
		detail := "differs from the config, and takes effect at the agent's next start (a running agent keeps the values lever wrote at its last start): " + strings.Join(differ, ", ")
		if len(good) > 0 {
			detail += "; as configured: " + strings.Join(good, ", ")
		}
		if view != "" {
			detail += "; " + view
		}
		if len(tail) > 0 {
			detail += "; " + strings.Join(tail, "; ")
		}
		fix := claudeSettingsFix
		if len(viewUnset) > 0 {
			fix += ". " + agentViewFix
		}
		return warnResult(check, detail, fix)
	}
	var parts []string
	if !configured {
		parts = append(parts, "none configured")
	}
	if len(good) > 0 {
		parts = append(parts, strings.Join(good, ", "))
	}
	if view != "" {
		parts = append(parts, view)
	}
	parts = append(parts, tail...)
	if len(viewUnset) > 0 {
		return warnResult(check, strings.Join(parts, "; "), agentViewFix)
	}
	if len(parts) == 0 {
		return checkResult{check, true, "no running agent", ""}
	}
	return checkResult{check, true, strings.Join(parts, "; "), ""}
}

// agentViewFix is the claude-settings row's fix when an agent's file does
// not turn agent view off. Boot rewrites the switch at every start, so a
// start fixes an agent that changed it; one that stays unset could not
// write /etc/claude-code.
const agentViewFix = "agent view not set means boot could not write the switch, or the agent changed it since its last start " +
	"(it owns /etc/claude-code in its container; lever rewrites it at every start): start the agent again. If it stays " +
	"not set, the image lacks a writable /etc/claude-code — check that the directory exists and the agent user can write " +
	"it, or rebuild the image (`make lever-image`) and recreate the agent on it"

// claudeSettingsFix is the claude-settings row's fix. A start applies the
// config's values; only an image that predates them needs a new container,
// and `lever stop && lever up` resumes the record on its old image.
const claudeSettingsFix = "start the agent again: `lever stop && lever up` for the manager, a stop and a resume for a worker. " +
	"If a start does not change the values, the agent image's lever-agent predates them: rebuild it (`make lever-image`), then " +
	"recreate the agent on it — the manager with `lever up --fresh --keep-conversation`, a worker " +
	"with `lever worker purge <name>` or a recycle. The manager can also have rewritten its own .lever/bootstrap.json; " +
	"`lever reload` stages a fresh one"

// claudeAgents is the manager and each worker with its configured Claude
// config, in the row's order.
func claudeAgents(manager string, managerWant *wire.Claude, workers []string, workerWant map[string]*wire.Claude) []claudeAgent {
	out := []claudeAgent{{manager, managerWant}}
	for _, w := range workers {
		out = append(out, claudeAgent{w, workerWant[w]})
	}
	return out
}
