package brokerctl

import (
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

// AgentMessages is remote.agent_messages as the broker enforces it: the
// limits, every contact's hub email and agents, and the ledger directory,
// which stays "" when the state directory is inside the tree (an agent could
// write a record there). Only a contact's agents list counts: a see-only
// agent and an operator login never appear.
func AgentMessages(app *config.App, st state.State) broker.AgentMessagesConfig {
	if !app.AgentMessagesOn() {
		return broker.AgentMessagesConfig{}
	}
	c := broker.AgentMessagesConfig{Enabled: true, FollowUpAfter: app.EffectiveAgentFollowUpAfter(), MaxChars: app.EffectiveAgentMaxChars()}
	if !StateInsideTree(app, st) {
		c.LedgerDir = st.AgentLedger()
	}
	for _, u := range app.Remote.AllowedUsers {
		if u.EffectiveTier() == config.TierContact {
			c.Contacts = append(c.Contacts, broker.ContactEntry{Login: u.Login,
				Email: strings.ToLower(config.HubEmailFor(u.Login)), Agents: slices.Clone(u.Agents)})
		}
	}
	return c
}
