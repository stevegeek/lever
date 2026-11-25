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
		"max open 1":         {func(a *App) { a.Remote.AgentMessages.MaxOpen = 1 }, ""},
		"max open 10":        {func(a *App) { a.Remote.AgentMessages.MaxOpen = 10 }, ""},
		"max open 11":        {func(a *App) { a.Remote.AgentMessages.MaxOpen = 11 }, "agent_messages.max_open 11"},
		"max open negative":  {func(a *App) { a.Remote.AgentMessages.MaxOpen = -1 }, "agent_messages.max_open -1"},
		"on, scion before the sender fix": {func(a *App) {
			a.Remote.AgentMessages.Enabled = true
			a.Scion.Version = "v0.0.0-20260827090000-0123456789ab"
		}, "b3562fb1"},
		"off, scion before the sender fix": {func(a *App) { a.Scion.Version = "v0.0.0-20260827090000-0123456789ab" }, ""},
		"on, scion at the sender fix": {func(a *App) {
			a.Remote.AgentMessages.Enabled = true
			a.Scion.Version = "v0.0.0-20260828114021-b3562fb19a97"
		}, ""},
		"on, scion bare hash": {func(a *App) {
			a.Remote.AgentMessages.Enabled = true
			a.Scion.Version = "63d5d65d"
		}, ""},
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

func TestScionVersionTime(t *testing.T) {
	for v, want := range map[string]string{
		"v0.0.0-20260828114021-b3562fb19a97":        "2026-08-28T11:40:21Z",
		"v1.2.4-0.20260901000000-0123456789ab":      "2026-09-01T00:00:00Z",
		"v1.3.0-rc.1.0.20260715120000-0123456789ab": "2026-07-15T12:00:00Z",
		"63d5d65d": "", "v1.2.3": "", "": "",
	} {
		got, ok := ScionVersionTime(v)
		if (want == "") == ok || ok && got.Format(time.RFC3339) != want {
			t.Errorf("%q: %v %v, want %q", v, got, ok, want)
		}
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
func TestRemotePushValidation(t *testing.T) {
	base := func() *App {
		return &App{Name: "boss", Tree: "/t", Workers: []Worker{{Name: "w1", Dir: "workers/w1"}},
			Remote: Remote{Enabled: true, BaseURL: "https://mac.ts.net", Landing: RemoteLandingChat,
				AllowedUsers: []RemoteUser{{Login: "op@x"}}}}
	}
	for name, tc := range map[string]struct {
		mut  func(a *App)
		want string
	}{
		"off":              {func(a *App) {}, ""},
		"off, subject set": {func(a *App) { a.Remote.Push.Subject = "junk" }, ""},
		"on mailto":        {func(a *App) { a.Remote.Push = Push{Enabled: true, Subject: "mailto:op@example.com"} }, ""},
		"on https":         {func(a *App) { a.Remote.Push = Push{Enabled: true, Subject: "https://example.com/contact"} }, ""},
		"no subject":       {func(a *App) { a.Remote.Push = Push{Enabled: true} }, "push.subject"},
		"http subject":     {func(a *App) { a.Remote.Push = Push{Enabled: true, Subject: "http://example.com"} }, "push.subject"},
		"mailto no at":     {func(a *App) { a.Remote.Push = Push{Enabled: true, Subject: "mailto:op"} }, "push.subject"},
		"mailto two":       {func(a *App) { a.Remote.Push = Push{Enabled: true, Subject: "mailto:a@b,c@d"} }, "push.subject"},
		"control char":     {func(a *App) { a.Remote.Push = Push{Enabled: true, Subject: "mailto:a@b\n"} }, "push.subject"},
		"test hosts": {func(a *App) {
			a.Remote.Push = Push{Enabled: true, Subject: "mailto:op@example.com", TestHosts: []string{"127.0.0.1:9447"}}
		}, ""},
		"test hosts, push off": {func(a *App) { a.Remote.Push = Push{TestHosts: []string{"127.0.0.1:9447"}} }, "push.test_hosts"},
		"test host not loopback": {func(a *App) {
			a.Remote.Push = Push{Enabled: true, Subject: "mailto:op@example.com", TestHosts: []string{"10.0.0.1:9447"}}
		}, "push.test_hosts"},
		"test host by name": {func(a *App) {
			a.Remote.Push = Push{Enabled: true, Subject: "mailto:op@example.com", TestHosts: []string{"localhost:9447"}}
		}, "push.test_hosts"},
		"console landing": {func(a *App) {
			a.Remote.Landing = ""
			a.Remote.Push = Push{Enabled: true, Subject: "mailto:op@example.com"}
		}, "landing: chat"},
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

func TestRemotePushYAML(t *testing.T) {
	a, err := LoadNoHostChecks(writeConfig(t, remoteWithWorkers+"  landing: chat\n  allowed_users:\n    - op@example.com\n"+
		"  push:\n    enabled: true\n    subject: mailto:op@example.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !a.PushOn() || a.Remote.Push.Subject != "mailto:op@example.com" {
		t.Fatalf("push %+v", a.Remote.Push)
	}
	a.Remote.Enabled = false
	if a.PushOn() {
		t.Fatal("push needs remote access on")
	}
}

func TestAgentMaxOpenYAMLAndOverrides(t *testing.T) {
	a, err := LoadNoHostChecks(writeConfig(t, remoteWithWorkers+"  allowed_users:\n    - op@example.com\n"+
		"    - {login: c@example.com, tier: contact, agents: [w1]}\n  agent_messages:\n    enabled: true\n    max_open: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := a.EffectiveAgentMaxOpen(""); got != 3 {
		t.Fatalf("instance max_open = %d, want 3", got)
	}
	if got := a.EffectiveAgentMaxOpen(a.Name); got != 3 {
		t.Fatalf("manager max_open = %d, want the instance's 3", got)
	}
	a.Manager.AgentMessages.MaxOpen = 5
	a.Workers[0].AgentMessages.MaxOpen = 1
	for agent, want := range map[string]int{"": 3, a.Name: 5, a.Workers[0].Name: 1, "nobody": 3} {
		if got := a.EffectiveAgentMaxOpen(agent); got != want {
			t.Errorf("EffectiveAgentMaxOpen(%q) = %d, want %d", agent, got, want)
		}
	}
	a.Remote.AgentMessages.MaxOpen = 0
	if got := a.EffectiveAgentMaxOpen("nobody"); got != DefaultAgentMaxOpen || DefaultAgentMaxOpen != 2 {
		t.Fatalf("default max_open = %d, want 2", got)
	}
}

func TestAgentMaxOpenOverrideValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(a *App)
		want string
	}{
		"manager 1":   {func(a *App) { a.Manager.AgentMessages.MaxOpen = 1 }, ""},
		"manager 11":  {func(a *App) { a.Manager.AgentMessages.MaxOpen = 11 }, "manager.agent_messages.max_open 11"},
		"worker 10":   {func(a *App) { a.Workers[0].AgentMessages.MaxOpen = 10 }, ""},
		"worker -1":   {func(a *App) { a.Workers[0].AgentMessages.MaxOpen = -1 }, "workers[w1].agent_messages.max_open -1"},
		"worker none": {func(a *App) {}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			a, err := LoadNoHostChecks(writeConfig(t, remoteWithWorkers+"  allowed_users:\n    - op@example.com\n"))
			if err != nil {
				t.Fatal(err)
			}
			tc.mut(a)
			err = a.Validate()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAgentMaxOpenPerAgentYAML(t *testing.T) {
	cfg := strings.Replace(remoteWithWorkers, "manager: {}", "manager: {agent_messages: {max_open: 4}}", 1)
	cfg = strings.Replace(cfg, "{name: w1, dir: workers/w1}", "{name: w1, dir: workers/w1, agent_messages: {max_open: 1}}", 1)
	a, err := LoadNoHostChecks(writeConfig(t, cfg+"  allowed_users:\n    - op@example.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a.EffectiveAgentMaxOpen(a.Name) != 4 || a.EffectiveAgentMaxOpen("w1") != 1 || a.EffectiveAgentMaxOpen("w2") != DefaultAgentMaxOpen {
		t.Fatalf("manager %d w1 %d w2 %d", a.EffectiveAgentMaxOpen(a.Name), a.EffectiveAgentMaxOpen("w1"), a.EffectiveAgentMaxOpen("w2"))
	}
	if _, err := LoadNoHostChecks(writeConfig(t, strings.Replace(cfg, "max_open: 1}", "max_open: 20}", 1)+"  allowed_users:\n    - op@example.com\n")); err == nil ||
		!strings.Contains(err.Error(), "workers[w1].agent_messages.max_open 20") {
		t.Fatalf("max_open 20: %v", err)
	}
}
