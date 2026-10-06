package brokerctl

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

func TestAgentMessagesConfig(t *testing.T) {
	app := &config.App{Name: "hello", Tree: t.TempDir(), Remote: config.Remote{Enabled: true,
		AllowedUsers:  []config.RemoteUser{{Login: "op@x"}, {Login: "C@Example.com", Tier: config.TierContact, Agents: []string{"w1"}}, {Login: "bob", Tier: config.TierContact, Agents: []string{"hello"}}},
		AgentMessages: config.AgentMessages{Enabled: true}}}
	st := state.State{Dir: t.TempDir()}
	got := AgentMessages(app, st)
	if !got.Enabled || got.FollowUpAfter != 24*time.Hour || got.MaxChars != 4000 || got.LedgerDir != st.AgentLedger() || len(got.Contacts) != 2 ||
		got.Contacts[0].Email != "c@example.com" || got.Contacts[1].Email != "bob@id.lever.local" {
		t.Fatalf("%+v", got)
	}
	app.Remote.AgentMessages.Enabled = false
	if AgentMessages(app, st).Enabled {
		t.Fatal("off")
	}
	inside := state.State{Dir: filepath.Join(app.Tree, ".lever-state")}
	app.Remote.AgentMessages.Enabled = true
	if AgentMessages(app, inside).LedgerDir != "" {
		t.Fatal("a state directory inside the tree must leave the ledger off")
	}
}

// Only agents: lists message access. A see-only agent and an operator login
// never reach the broker's contact list.
func TestAgentMessagesConfigExcludesSeeOnlyAndOperators(t *testing.T) {
	app := &config.App{Name: "hello", Tree: t.TempDir(), Remote: config.Remote{Enabled: true,
		AllowedUsers:  []config.RemoteUser{{Login: "op@x"}, {Login: "c@x", Tier: config.TierContact, Agents: []string{"w1"}, See: []string{"w2"}}},
		AgentMessages: config.AgentMessages{Enabled: true}}}
	got := AgentMessages(app, state.State{Dir: t.TempDir()})
	if len(got.Contacts) != 1 || got.Contacts[0].Login != "c@x" || len(got.Contacts[0].Agents) != 1 || got.Contacts[0].Agents[0] != "w1" {
		t.Fatalf("contacts = %+v", got.Contacts)
	}
}
