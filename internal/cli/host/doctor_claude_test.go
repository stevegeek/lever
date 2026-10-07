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
	asConfigured := jail.ClaudeDelivered{AutoCompactWindow: 400000, NoteSum: jail.NoteSum("Re-read NOTES.md")}

	r := checkClaudeSettings(context.Background(), "/lever", claudeAgents("assistant", nil, []string{"scratch"}, nil), fleet, nil)
	if !r.ok || r.detail != "none configured" {
		t.Fatalf("nothing configured reads nothing: %+v", r)
	}

	r = checkClaudeSettings(context.Background(), "/lever", agents, fleet, delivered(map[string]jail.ClaudeDelivered{
		"lever--assistant": asConfigured, "lever--scratch": {}}))
	if !r.ok || r.fix != "" || !strings.Contains(r.detail, "assistant (window 400000, after-compact note)") ||
		!strings.Contains(r.detail, "not running (applied at start): idle") || strings.Contains(r.detail, "scratch") {
		t.Fatalf("as configured: %+v", r)
	}

	// The running manager has the values of its last start.
	r = checkClaudeSettings(context.Background(), "/lever", agents, fleet, delivered(map[string]jail.ClaudeDelivered{
		"lever--assistant": {AutoCompactWindow: 300000}, "lever--scratch": {}}))
	if !r.ok || r.fix == "" || !strings.Contains(r.detail, "assistant (configured window 400000, after-compact note; has window 300000)") {
		t.Fatalf("differs: %+v", r)
	}

	// Same shape, another note.
	r = checkClaudeSettings(context.Background(), "/lever", agents, fleet, delivered(map[string]jail.ClaudeDelivered{
		"lever--assistant": {AutoCompactWindow: 400000, NoteSum: jail.NoteSum("old")}, "lever--scratch": {}}))
	if r.fix == "" || !strings.Contains(r.detail, "but a different note") {
		t.Fatalf("other note: %+v", r)
	}

	// A leftover on a worker whose config was removed is a difference too.
	r = checkClaudeSettings(context.Background(), "/lever", agents, fleet, delivered(map[string]jail.ClaudeDelivered{
		"lever--assistant": asConfigured, "lever--scratch": {AutoCompactWindow: 500000}}))
	if r.fix == "" || !strings.Contains(r.detail, "scratch (configured none; has window 500000)") {
		t.Fatalf("leftover: %+v", r)
	}

	// An unreadable container is named by class, never by its output.
	r = checkClaudeSettings(context.Background(), "/lever", agents, fleet, delivered(map[string]jail.ClaudeDelivered{
		"lever--scratch": {}}))
	if !r.ok || !strings.Contains(r.detail, "not checked (unreadable): assistant (unexpected output)") {
		t.Fatalf("unreadable: %+v", r)
	}
}
