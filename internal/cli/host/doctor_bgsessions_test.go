package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
)

func TestCheckBackgroundSessions(t *testing.T) {
	fleet := func(context.Context, string) ([]scion.Agent, error) {
		return []scion.Agent{
			{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days"},
			{Slug: "scratch", Phase: "running", ContainerStatus: "Up 1 hour"},
			{Slug: "idle", Phase: "suspended", ContainerStatus: "stopped"},
		}, nil
	}
	agents := []string{"assistant", "scratch", "idle"}
	var asked []string
	reader := func(m map[string]jail.BackgroundRecords) bgSessionsReader {
		asked = nil
		return func(_ context.Context, ref string) (jail.BackgroundRecords, error) {
			asked = append(asked, ref)
			r, ok := m[ref]
			if !ok {
				return jail.BackgroundRecords{}, jail.ErrSessionsShape
			}
			return r, nil
		}
	}

	r := checkBackgroundSessions(context.Background(), "/lever", agents, fleet, reader(map[string]jail.BackgroundRecords{
		"lever--assistant": {}, "lever--scratch": {}}))
	if !r.ok || r.fix != "" || r.detail != "none stale" {
		t.Fatalf("clean: %+v", r)
	}
	if len(asked) != 2 {
		t.Fatalf("probed %q, want only the running agents", asked)
	}

	r = checkBackgroundSessions(context.Background(), "/lever", agents, fleet, reader(map[string]jail.BackgroundRecords{
		"lever--assistant": {Stale: 2, Unreadable: 1}}))
	if !r.ok || r.fix != bgSessionsFix || !strings.Contains(r.detail, "assistant (2)") ||
		!strings.Contains(r.detail, "block the next claude --continue") ||
		!strings.Contains(r.detail, "scratch (unexpected output)") || !strings.Contains(r.detail, "assistant: 1 unreadable") {
		t.Fatalf("stale: %+v", r)
	}

	r = checkBackgroundSessions(context.Background(), "/lever", agents, func(context.Context, string) ([]scion.Agent, error) {
		return nil, errors.New("hub down")
	}, reader(nil))
	if !r.ok || !strings.HasPrefix(r.detail, "not checked (could not list agents)") {
		t.Fatalf("list error: %+v", r)
	}
	if r := checkBackgroundSessions(context.Background(), "/lever", agents, fleet, nil); r.detail != "not checked" {
		t.Fatalf("nil reader: %+v", r)
	}
	none := func(context.Context, string) ([]scion.Agent, error) { return nil, nil }
	if r := checkBackgroundSessions(context.Background(), "/lever", agents, none, reader(nil)); r.detail != "no running agent" {
		t.Fatalf("no agents: %+v", r)
	}
}
