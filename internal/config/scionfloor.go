package config

import (
	"regexp"
	"time"
)

// AgentMessagesScionFloor is the first scion commit whose hub sets a
// message's sender from the caller's auth (b3562fb1, #1343, 2026-08-28).
// The agent-messages filter trusts that sender: on an older hub an agent can
// post a row as its contact and the filter shows it. agentMessagesScionTime
// is its commit time, the time a pseudo-version of it carries.
const AgentMessagesScionFloor = "b3562fb19a970a7788bb7aa110c024799506f225"

var agentMessagesScionTime = time.Date(2026, 8, 28, 11, 40, 21, 0, time.UTC)

// pseudoVersionRE is the commit time and hash ending a Go pseudo-version
// (v0.0.0-20260828114021-b3562fb19a97, vX.Y.Z-pre.0.<time>-<hash>, ...).
var pseudoVersionRE = regexp.MustCompile(`[-.]([0-9]{14})-[0-9a-f]{12}$`)

// ScionVersionTime is the commit time a scion.version pseudo-version
// carries; ok is false for a bare commit hash or a tag, whose time lever
// cannot tell without the module proxy.
func ScionVersionTime(v string) (time.Time, bool) {
	m := pseudoVersionRE.FindStringSubmatch(v)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102150405", m[1])
	return t, err == nil
}

// ScionVersionPredatesAgentMessages reports whether scion.version is a
// pseudo-version older than AgentMessagesScionFloor. A commit time at or
// after it passes: lever pins upstream main, where a later commit holds it.
func (a *App) ScionVersionPredatesAgentMessages() bool {
	t, ok := ScionVersionTime(a.Scion.Version)
	return ok && t.Before(agentMessagesScionTime)
}
