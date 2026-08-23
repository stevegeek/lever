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

// SaveRevocation persists the revocation state (0600), atomically.
func SaveRevocation(st state.State, rs broker.RevocationState) error {
	return state.SaveJSON(st.Revocation(), "revocation", rs)
}

// LoadDirectives reads persisted directive state; absent file = zero value.
func LoadDirectives(st state.State) (broker.DirectiveState, error) {
	return state.LoadJSON[broker.DirectiveState](st.Directives(), "directives")
}

// SaveDirectives persists directive state (0600), atomically: a crash
// mid-write must never torn-write directives.json, since it holds the
// replay tombstone set the broker needs on restart.
func SaveDirectives(st state.State, ds broker.DirectiveState) error {
	return state.SaveJSON(st.Directives(), "directives", ds)
}
