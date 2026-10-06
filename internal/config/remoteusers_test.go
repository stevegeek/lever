package config

import (
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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

func TestRemoteSeeAndLabelsValidation(t *testing.T) {
	base := func() *App {
		return &App{Name: "boss", Tree: "/t", Workers: []Worker{{Name: "w1", Dir: "workers/w1"}, {Name: "w2", Dir: "workers/w2"}},
			Remote: Remote{Enabled: true, BaseURL: "https://mac.ts.net", Landing: RemoteLandingChat,
				AllowedUsers: []RemoteUser{{Login: "op@x"}, {Login: "c@x", Tier: TierContact, Agents: []string{"w1"}}}}}
	}
	for name, tc := range map[string]struct {
		mut  func(a *App)
		want string // "" = valid
	}{
		"see a worker":            {func(a *App) { a.Remote.AllowedUsers[1].See = []string{"w2"} }, ""},
		"see the manager":         {func(a *App) { a.Remote.AllowedUsers[1].See = []string{"boss"} }, ""},
		"see unknown":             {func(a *App) { a.Remote.AllowedUsers[1].See = []string{"zz"} }, "not a declared worker or the manager"},
		"see and agents":          {func(a *App) { a.Remote.AllowedUsers[1].See = []string{"w1"} }, "both agents and see"},
		"see twice":               {func(a *App) { a.Remote.AllowedUsers[1].See = []string{"w2", "w2"} }, "twice"},
		"see on operator":         {func(a *App) { a.Remote.AllowedUsers[0].See = []string{"w2"} }, "operator"},
		"labels ok":               {func(a *App) { a.Remote.LabelsFile = "workers/labels.json" }, ""},
		"labels absolute":         {func(a *App) { a.Remote.LabelsFile = "/etc/passwd" }, "labels_file"},
		"labels dotdot":           {func(a *App) { a.Remote.LabelsFile = "../x.json" }, "labels_file"},
		"labels inner dotdot":     {func(a *App) { a.Remote.LabelsFile = "a/../../x.json" }, "labels_file"},
		"labels unclean":          {func(a *App) { a.Remote.LabelsFile = "a//b.json" }, "labels_file"},
		"labels dot":              {func(a *App) { a.Remote.LabelsFile = "." }, "labels_file"},
		"labels backslash":        {func(a *App) { a.Remote.LabelsFile = `a\b.json` }, "labels_file"},
		"labels in a worker":      {func(a *App) { a.Remote.LabelsFile = "workers/w1/labels.json" }, "worker"},
		"labels is a worker":      {func(a *App) { a.Remote.LabelsFile = "workers/w2" }, "worker"},
		"labels worker case":      {func(a *App) { a.Remote.LabelsFile = "Workers/W1/labels.json" }, "worker"},
		"labels worker peer":      {func(a *App) { a.Remote.LabelsFile = "workers/w10.json" }, ""},
		"labels in .lever":        {func(a *App) { a.Remote.LabelsFile = ".lever/bootstrap.json" }, "host"},
		"labels in state":         {func(a *App) { a.Remote.LabelsFile = "x/.lever-state/a.json" }, "host"},
		"labels .lever case":      {func(a *App) { a.Remote.LabelsFile = ".LEVER/a.json" }, "host"},
		"labels under state tree": {func(a *App) { a.dir, a.Tree, a.Remote.LabelsFile = "/r", "/r/.lever-state", "labels.json" }, "host"},
	} {
		t.Run(name, func(t *testing.T) {
			a := base()
			tc.mut(a)
			err := a.validateRemote()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRemoteUserYAMLAcceptsSee(t *testing.T) {
	var u RemoteUser
	if err := yaml.Unmarshal([]byte("{login: c@x, tier: contact, agents: [w1], see: [w2]}"), &u); err != nil || !slices.Equal(u.See, []string{"w2"}) {
		t.Fatalf("see: %v %+v", err, u)
	}
}

func TestRemoteAgentMessagesValidation(t *testing.T) {
	base := func() *App {
		return &App{Name: "boss", Tree: "/t", Workers: []Worker{{Name: "w1", Dir: "workers/w1"}},
			Remote: Remote{Enabled: true, BaseURL: "https://mac.ts.net",
				AllowedUsers: []RemoteUser{{Login: "op@x"}, {Login: "c@x", Tier: TierContact, Agents: []string{"w1"}}}}}
	}
	for name, tc := range map[string]struct {
		mut  func(a *App)
		want string
	}{
		"off": {func(a *App) {}, ""},
		"on":  {func(a *App) { a.Remote.AgentMessages.Enabled = true }, ""},
		"on no contact": {func(a *App) {
			a.Remote.AgentMessages.Enabled = true
			a.Remote.AllowedUsers = a.Remote.AllowedUsers[:1]
		}, "contact"},
		"follow up 2h":       {func(a *App) { a.Remote.AgentMessages.FollowUpAfter = 2 * time.Hour }, ""},
		"follow up 10m":      {func(a *App) { a.Remote.AgentMessages.FollowUpAfter = 10 * time.Minute }, "follow_up_after"},
		"follow up 31d":      {func(a *App) { a.Remote.AgentMessages.FollowUpAfter = 31 * 24 * time.Hour }, "follow_up_after"},
		"max chars 16000":    {func(a *App) { a.Remote.AgentMessages.MaxChars = 16000 }, ""},
		"max chars 16001":    {func(a *App) { a.Remote.AgentMessages.MaxChars = 16001 }, "max_chars"},
		"max chars negative": {func(a *App) { a.Remote.AgentMessages.MaxChars = -1 }, "max_chars"},
	} {
		t.Run(name, func(t *testing.T) {
			a := base()
			tc.mut(a)
			err := a.validateRemote()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRemoteAgentMessagesYAMLAndDefaults(t *testing.T) {
	a, err := LoadNoHostChecks(writeConfig(t, remoteWithWorkers+"  allowed_users:\n    - op@example.com\n"+
		"    - {login: c@example.com, tier: contact, agents: [w1]}\n  agent_messages:\n    enabled: true\n    follow_up_after: 48h\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !a.AgentMessagesOn() || a.EffectiveAgentFollowUpAfter() != 48*time.Hour || a.EffectiveAgentMaxChars() != DefaultAgentMaxChars {
		t.Fatalf("on=%v follow=%v max=%d", a.AgentMessagesOn(), a.EffectiveAgentFollowUpAfter(), a.EffectiveAgentMaxChars())
	}
	a.Remote.Enabled = false
	if a.AgentMessagesOn() {
		t.Fatal("agent messages need remote access on")
	}
}
