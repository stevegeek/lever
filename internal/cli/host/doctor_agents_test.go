package host

import (
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/config"
)

func TestCheckSpawnedAgents(t *testing.T) {
	app := &config.App{Templates: []config.Template{{Name: "session"}}}
	if r := checkSpawnedAgents(app, broker.AgentsState{}, nil); !r.ok || r.fix != "" {
		t.Fatalf("%+v", r)
	}
	st := broker.AgentsState{Agents: []broker.AgentRecord{
		{Name: "s-aaaaaa", Template: "session", State: broker.AgentActive},
		{Name: "s-bbbbbb", Template: "session", State: broker.AgentDestroying},
		{Name: "x-cccccc", Template: "gone", State: broker.AgentActive},
		{Name: "s-dddddd", Template: "session", State: broker.AgentDestroyed},
	}}
	r := checkSpawnedAgents(app, st, nil)
	if !r.ok || !strings.Contains(r.detail, "3 spawned agent(s)") || !strings.Contains(r.detail, "s-bbbbbb (destroying)") || !strings.Contains(r.detail, "x-cccccc") || r.fix == "" {
		t.Fatalf("%+v", r)
	}
	if got := liveSpawned(st); len(got) != 3 {
		t.Fatalf("%v", got)
	}
	if r := checkSpawnedAgents(app, st, errors.New("parse")); r.ok {
		t.Fatal("an unreadable registry passed")
	}
}
