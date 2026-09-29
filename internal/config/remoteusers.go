package config

import (
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

// Remote user tiers: what a verified web chat message from the login counts
// as (see package chatledger), and which hub role and proxy fence apply.
const (
	// TierOperator is the operator's own login: every agent, the full
	// lever-remote web role, and verified chat that is operator steering.
	TierOperator = "operator"
	// TierContact is an external contact: chat only (no terminal, no
	// start/stop), only with the agents it lists, and verified chat that is
	// the contact's answer for the task, never operator steering.
	TierContact = "contact"
)

// RemoteUser is one allowed_users entry. The plain string form is a login
// with tier operator (the meaning before tiers existed); the map form names
// the tier and, for a contact, the agents it may reach.
type RemoteUser struct {
	Login string `yaml:"login"`
	// Tier is TierOperator or TierContact; "" means TierOperator.
	Tier string `yaml:"tier"`
	// Agents is the agents a contact may reach, by name (declared workers,
	// or the app name for the manager). Required for a contact, refused for
	// an operator: a contact's reach is always explicit, so a new worker
	// never widens it by accident.
	Agents []string `yaml:"agents"`
}

// UnmarshalYAML accepts a plain login or a {login, tier, agents} map, and
// refuses any other key in the map.
func (u *RemoteUser) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&u.Login)
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("config: remote: allowed_users entries are a login or a {login, tier, agents} map (line %d)", n.Line)
	}
	for i := 0; i < len(n.Content); i += 2 {
		if k := n.Content[i].Value; !slices.Contains([]string{"login", "tier", "agents"}, k) {
			return fmt.Errorf("config: remote: allowed_users entry has unknown key %q (line %d); keys are login, tier, agents", k, n.Content[i].Line)
		}
	}
	type plain RemoteUser
	return n.Decode((*plain)(u))
}

// EffectiveTier is Tier with "" read as TierOperator.
func (u RemoteUser) EffectiveTier() string {
	if u.Tier == "" {
		return TierOperator
	}
	return u.Tier
}

// Logins is every allowed login, in config order.
func (r Remote) Logins() []string {
	out := make([]string, len(r.AllowedUsers))
	for i, u := range r.AllowedUsers {
		out[i] = u.Login
	}
	return out
}

// LoginsWithTier is the allowed logins of one tier, in config order.
func (r Remote) LoginsWithTier(tier string) []string {
	var out []string
	for _, u := range r.AllowedUsers {
		if u.EffectiveTier() == tier {
			out = append(out, u.Login)
		}
	}
	return out
}
