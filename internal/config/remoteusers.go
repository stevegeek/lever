package config

import (
	"fmt"
	"slices"
	"strings"

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
	// See is the agents a contact may see on the chat page (name, label,
	// state) but not message: no history, no input, no id. Contact only;
	// disjoint from Agents.
	See []string `yaml:"see"`
}

// UnmarshalYAML accepts a plain login or a {login, tier, agents, see} map, and
// refuses any other key in the map.
func (u *RemoteUser) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&u.Login)
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("config: remote: allowed_users entries are a login or a {login, tier, agents, see} map (line %d)", n.Line)
	}
	for i := 0; i < len(n.Content); i += 2 {
		if k := n.Content[i].Value; !slices.Contains([]string{"login", "tier", "agents", "see"}, k) {
			return fmt.Errorf("config: remote: allowed_users entry has unknown key %q (line %d); keys are login, tier, agents, see", k, n.Content[i].Line)
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

// HubEmailFor is the hub user email a remote sign-in as login becomes
// (remoteproxy's identityFor): the login itself when it contains "@", else
// <login>@id.lever.local; the unnamed operator (login "", when allowed_users
// is empty) is lever-operator@lever.local.
func HubEmailFor(login string) string {
	switch {
	case login == "":
		return remoteUnnamedOperatorKey
	case strings.Contains(login, "@"):
		return login
	}
	return login + "@" + remoteIDEmailDomain
}

// HubUserEmails is the set of hub user emails the proxy's sign-ins can
// create: one per allowed login, or the single unnamed operator when the
// list is empty.
func (r Remote) HubUserEmails() []string {
	if len(r.AllowedUsers) == 0 {
		return []string{HubEmailFor("")}
	}
	out := make([]string, 0, len(r.AllowedUsers))
	for _, u := range r.AllowedUsers {
		out = append(out, HubEmailFor(u.Login))
	}
	return out
}

// WebSenders is the envelope sender each of those sign-ins posts as: "user:"
// plus the hub email, lowercased (the hub lowercases emails), sorted. A
// message an agent receives from one of them can only be a web chat post.
func (r Remote) WebSenders() []string {
	var out []string
	for _, e := range r.HubUserEmails() {
		out = append(out, "user:"+strings.ToLower(e))
	}
	slices.Sort(out)
	return slices.Compact(out)
}
