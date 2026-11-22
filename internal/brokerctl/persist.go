package brokerctl

import (
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/state"
)

// LoadRevocation reads the persisted revocation state; an absent file is the
// zero value (epoch 0, no revocations).
func LoadRevocation(st state.State) (broker.RevocationState, error) {
	return state.LoadJSON[broker.RevocationState](st.Revocation(), "revocation")
}

func SaveRevocation(st state.State, rs broker.RevocationState) error {
	return state.SaveJSON(st.Revocation(), "revocation", rs)
}

func LoadDirectives(st state.State) (broker.DirectiveState, error) {
	return state.LoadJSON[broker.DirectiveState](st.Directives(), "directives")
}

// SaveDirectives persists directive state atomically (0600): it holds the
// replay tombstone set the broker needs on restart.
func SaveDirectives(st state.State, ds broker.DirectiveState) error {
	return state.SaveJSON(st.Directives(), "directives", ds)
}

// LoadAgents reads the spawned-agent registry; an absent file is empty. A
// file that does not parse is an error: the broker must not start without
// the record of the names it has used.
func LoadAgents(st state.State) (broker.AgentsState, error) {
	return state.LoadJSON[broker.AgentsState](st.Agents(), "agents")
}

func SaveAgents(st state.State, as broker.AgentsState) error {
	return state.SaveJSON(st.Agents(), "agents", as)
}
