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

// RemoteUser is one allowed_users entry: a plain login (tier operator) or a
// map naming the tier and, for a contact, the agents it may reach.
type RemoteUser struct {
	Login string `yaml:"login"`
	// Tier is TierOperator or TierContact; "" means TierOperator.
	Tier string `yaml:"tier"`
	// Agents is required for a contact and refused for an operator: a
	// contact's reach is always explicit, so a new worker never widens it by
	// accident.
	Agents []string `yaml:"agents"`
	// See is the agents a contact may see on the chat page (name, label,
	// state) but not message: no history, no input, no id. Contact only;
	// disjoint from Agents.
	See []string `yaml:"see"`
	// Files and Voice false turn that feature off for this login.
	Files *bool `yaml:"files"`
	Voice *bool `yaml:"voice"`
}

// FilesAllowed is Files with nil read as true.
func (u RemoteUser) FilesAllowed() bool { return u.Files == nil || *u.Files }

// VoiceAllowed is Voice with nil read as true.
func (u RemoteUser) VoiceAllowed() bool { return u.Voice == nil || *u.Voice }

// UnmarshalYAML refuses unknown keys in the map form.
func (u *RemoteUser) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&u.Login)
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("config: remote: allowed_users entries are a login or a {login, tier, agents, see, files, voice} map (line %d)", n.Line)
	}
	for i := 0; i < len(n.Content); i += 2 {
		if k := n.Content[i].Value; !slices.Contains([]string{"login", "tier", "agents", "see", "files", "voice"}, k) {
			return fmt.Errorf("config: remote: allowed_users entry has unknown key %q (line %d); keys are login, tier, agents, see, files, voice", k, n.Content[i].Line)
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

// ScionDevUserID is scion's fixed well-known UUID for the development
// pseudo-user (scion pkg/hub/devauth.go). On every lever instance this row is
// seeded super-admin and nothing removes it; lever uses it only as its
// host-side controller identity, never for a remote human. scion's workstation
// PUT /api/v1/system/identity route accepts any authenticated loopback caller
// with no permission check, so any jailed agent can rename this row to the
// email of an operator or contact who has no hub user yet; that email's first
// sign-in would then resolve to the super-admin row. So no remote sign-in may
// resolve to this id.
const ScionDevUserID = "be67fbc9-c869-5d43-b15d-c28ca3e8d355"

// HubEmailFor is the hub user email a remote sign-in as login becomes; it
// must agree with remoteproxy's identityFor.
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
// create.
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

// WebSenders is the envelope sender each sign-in posts as. A message an
// agent receives from one of them can only be a web chat post.
func (r Remote) WebSenders() []string {
	var out []string
	for _, e := range r.HubUserEmails() {
		out = append(out, "user:"+strings.ToLower(e))
	}
	slices.Sort(out)
	return slices.Compact(out)
}
