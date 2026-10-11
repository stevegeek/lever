package host

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

func TestCheckClaudeSettings(t *testing.T) {
	fleet := func(context.Context, string) ([]scion.Agent, error) {
		return []scion.Agent{
			{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days"},
			{Slug: "scratch", Phase: "running", ContainerStatus: "Up 1 hour"},
			{Slug: "idle", Phase: "suspended", ContainerStatus: "stopped"},
		}, nil
	}
	mgr := &wire.Claude{AutoCompactWindow: 400000, AfterCompactNote: "Re-read NOTES.md"}
	idle := &wire.Claude{AutoCompactWindow: 200000}
	agents := claudeAgents("assistant", mgr, []string{"scratch", "idle"}, map[string]*wire.Claude{"idle": idle})
	delivered := func(m map[string]jail.ClaudeDelivered) claudeSettingsReader {
		return func(_ context.Context, ref string) (jail.ClaudeDelivered, error) {
			d, ok := m[ref]
			if !ok {
				return jail.ClaudeDelivered{}, jail.ErrClaudeShape
			}
			return d, nil
		}
	}
	asConfigured := jail.ClaudeDelivered{AutoCompactWindow: 400000, NoteSum: jail.NoteSum("Re-read NOTES.md"), AgentViewOff: true}
	off := jail.ClaudeDelivered{AgentViewOff: true}

	r := checkClaudeSettings(context.Background(), "/lever", claudeAgents("assistant", nil, []string{"scratch"}, nil), fleet, nil)
	if !r.ok || r.detail != "none configured" {
		t.Fatalf("nothing configured reads nothing: %+v", r)
	}

	r = checkClaudeSettings(context.Background(), "/lever", agents, fleet, delivered(map[string]jail.ClaudeDelivered{
		"lever--assistant": asConfigured, "lever--scratch": off}))
	if !r.ok || r.fix != "" || !strings.Contains(r.detail, "assistant (window 400000, after-compact note)") ||
		!strings.Contains(r.detail, "not running (applied at start): idle") || strings.Contains(r.detail, "scratch") ||
		!strings.Contains(r.detail, "agent view: off") {
		t.Fatalf("as configured: %+v", r)
	}

	// The running manager has the values of its last start.
	r = checkClaudeSettings(context.Background(), "/lever", agents, fleet, delivered(map[string]jail.ClaudeDelivered{
		"lever--assistant": {AutoCompactWindow: 300000, AgentViewOff: true}, "lever--scratch": off}))
	if !r.ok || !strings.Contains(r.detail, "assistant (configured window 400000, after-compact note; has window 300000)") ||
		!strings.Contains(r.detail, "takes effect at the agent's next start") {
		t.Fatalf("differs: %+v", r)
	}
	// An old image is fixed by a new container, never by stop+up alone.
	for _, want := range []string{"make lever-image", "lever up --fresh", "back up its conversation", "lever worker purge <name>", "recycle"} {
		if !strings.Contains(r.fix, want) {
			t.Fatalf("fix %q lacks %q", r.fix, want)
		}
	}

	// Same shape, another note.
	r = checkClaudeSettings(context.Background(), "/lever", agents, fleet, delivered(map[string]jail.ClaudeDelivered{
		"lever--assistant": {AutoCompactWindow: 400000, NoteSum: jail.NoteSum("old"), AgentViewOff: true}, "lever--scratch": off}))
	if r.fix == "" || !strings.Contains(r.detail, "but a different note") {
		t.Fatalf("other note: %+v", r)
	}

	// A leftover on a worker whose config was removed is a difference too.
	r = checkClaudeSettings(context.Background(), "/lever", agents, fleet, delivered(map[string]jail.ClaudeDelivered{
		"lever--assistant": asConfigured, "lever--scratch": {AutoCompactWindow: 500000, AgentViewOff: true}}))
	if r.fix == "" || !strings.Contains(r.detail, "scratch (configured none; has window 500000)") {
		t.Fatalf("leftover: %+v", r)
	}

	// Config removed: nothing configured, but a running agent still holds
	// what lever wrote at its last start.
	none := claudeAgents("assistant", nil, []string{"scratch"}, nil)
	r = checkClaudeSettings(context.Background(), "/lever", none, fleet, delivered(map[string]jail.ClaudeDelivered{
		"lever--assistant": {AutoCompactWindow: 400000, AgentViewOff: true}, "lever--scratch": off}))
	if r.fix == "" || !strings.Contains(r.detail, "assistant (configured none; has window 400000)") || !strings.Contains(r.detail, "next start") {
		t.Fatalf("removed config: %+v", r)
	}
	r = checkClaudeSettings(context.Background(), "/lever", none, fleet, delivered(map[string]jail.ClaudeDelivered{
		"lever--assistant": off, "lever--scratch": off}))
	if !r.ok || r.fix != "" || r.detail != "none configured; agent view: off" {
		t.Fatalf("nothing configured, nothing delivered: %+v", r)
	}

	// No switch in an agent's file (an image without a writable
	// /etc/claude-code): a warning naming the agent, with its own fix.
	r = checkClaudeSettings(context.Background(), "/lever", none, fleet, delivered(map[string]jail.ClaudeDelivered{
		"lever--assistant": off, "lever--scratch": {}}))
	if r.fix == "" || !strings.Contains(r.detail, "agent view: not set (background sessions possible): scratch") ||
		strings.Contains(r.detail, "agent view: off") || strings.Contains(r.detail, "assistant") {
		t.Fatalf("switch not set: %+v", r)
	}
	for _, want := range []string{"/etc/claude-code", "rebuild the image", "make lever-image", "every start"} {
		if !strings.Contains(r.fix, want) {
			t.Fatalf("fix %q lacks %q", r.fix, want)
		}
	}
	if strings.Contains(r.fix, "lever worker purge") {
		t.Fatalf("a switch-only finding got the config fix: %q", r.fix)
	}
	// Both findings: both fixes.
	r = checkClaudeSettings(context.Background(), "/lever", agents, fleet, delivered(map[string]jail.ClaudeDelivered{
		"lever--assistant": {AutoCompactWindow: 300000}, "lever--scratch": off}))
	if !strings.Contains(r.detail, "has window 300000") || !strings.Contains(r.detail, "agent view: not set (background sessions possible): assistant") ||
		!strings.Contains(r.fix, "lever worker purge") || !strings.Contains(r.fix, "/etc/claude-code") {
		t.Fatalf("both findings: %+v", r)
	}

	// An unreadable container is named by class, never by its output.
	r = checkClaudeSettings(context.Background(), "/lever", agents, fleet, delivered(map[string]jail.ClaudeDelivered{
		"lever--scratch": off}))
	if !r.ok || !strings.Contains(r.detail, "not checked (unreadable): assistant (unexpected output)") || !strings.Contains(r.detail, "agent view: off") {
		t.Fatalf("unreadable: %+v", r)
	}
}
