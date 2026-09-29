package config

import (
	"slices"
	"strings"
	"testing"
)

// remoteWithWorkers is remoteOn plus two declared workers.
const remoteWithWorkers = "name: x\nbackend: lima\ntree: ./tree\nmanager: {}\n" +
	"workers:\n  - {name: w1, dir: workers/w1}\n  - {name: w2, dir: workers/w2}\n" +
	"remote:\n  enabled: true\n  base_url: \"https://vm.exe.xyz:8445\"\n"

func TestAllowedUsersTiers(t *testing.T) {
	app, err := LoadNoHostChecks(writeConfig(t, remoteWithWorkers+
		"  allowed_users:\n    - op@example.com\n    - {login: c@example.com, tier: contact, agents: [w1, x]}\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := app.Remote.Logins(); !slices.Equal(got, []string{"op@example.com", "c@example.com"}) {
		t.Fatalf("logins %v", got)
	}
	op, c := app.Remote.AllowedUsers[0], app.Remote.AllowedUsers[1]
	if op.EffectiveTier() != TierOperator || c.EffectiveTier() != TierContact || !slices.Equal(c.Agents, []string{"w1", "x"}) {
		t.Fatalf("entries %+v %+v", op, c)
	}
	if got := app.Remote.LoginsWithTier(TierContact); !slices.Equal(got, []string{"c@example.com"}) {
		t.Fatalf("contacts %v", got)
	}
}

func TestAllowedUsersTierValidation(t *testing.T) {
	for entry, want := range map[string]string{
		"{login: c@example.com, tier: contact}":                   "must list its agents",
		"{login: c@example.com, tier: contact, agents: [nope]}":   "not a declared worker",
		"{login: c@example.com, tier: contact, agents: [w1, w1]}": "twice",
		"{login: c@example.com, tier: admin, agents: [w1]}":       "use operator or contact",
		"{login: c@example.com, agents: [w1]}":                    "operator, which reaches every agent",
		"{login: c@example.com, tier: contact, agent: [w1]}":      "unknown key",
	} {
		_, err := LoadNoHostChecks(writeConfig(t, remoteWithWorkers+"  allowed_users:\n    - "+entry+"\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", entry, err, want)
		}
	}
}
