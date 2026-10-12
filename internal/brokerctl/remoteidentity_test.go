package brokerctl

import (
	"reflect"
	"slices"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

// TestRemoteConfigHashTracksRemoteOnly: the hash must move when anything the
// PROXY reads changes, and must NOT move for config the proxy never reads —
// otherwise every unrelated edit would bounce the proxy and drop live sessions.
func TestRemoteConfigHashTracksRemoteOnly(t *testing.T) {
	base := &config.App{Remote: config.Remote{Enabled: true, Port: 8445, BaseURL: "https://h.ts.net"}}
	h := RemoteConfigHash(base)

	same := &config.App{Remote: config.Remote{Enabled: true, Port: 8445, BaseURL: "https://h.ts.net"}}
	if RemoteConfigHash(same) != h {
		t.Error("identical remote config must hash identically, or every apply restarts the proxy")
	}

	for name, mutate := range map[string]func(*config.App){
		"allowed_users":        func(a *config.App) { a.Remote.AllowedUsers = []config.RemoteUser{{Login: "me@example.com"}} },
		"base_url":             func(a *config.App) { a.Remote.BaseURL = "https://other.ts.net" },
		"port":                 func(a *config.App) { a.Remote.Port = 9445 },
		"login_port":           func(a *config.App) { a.Remote.LoginPort = 8449 },
		"enabled":              func(a *config.App) { a.Remote.Enabled = false },
		"identity_header":      func(a *config.App) { a.Remote.IdentityHeader = "X-ExeDev-Email" },
		"bind":                 func(a *config.App) { a.Remote.Bind = "10.0.0.5" },
		"allow_wildcard_bind":  func(a *config.App) { a.Remote.AllowWildcardBind = true },
		"trust_forwarded_host": func(a *config.App) { a.Remote.TrustForwardedHost = true },
		"landing":              func(a *config.App) { a.Remote.Landing = config.RemoteLandingChat },
	} {
		changed := &config.App{Remote: base.Remote}
		mutate(changed)
		if RemoteConfigHash(changed) == h {
			t.Errorf("changing %s did not change the hash — the running proxy would keep the old value", name)
		}
	}

	// Writing the default out, or the header in another case, is not a change.
	spelled := &config.App{Remote: base.Remote}
	spelled.Remote.IdentityHeader = "tailscale-user-login"
	spelled.Remote.Bind = "127.0.0.1"
	spelled.Remote.Landing = config.RemoteLandingConsole
	if RemoteConfigHash(spelled) != h {
		t.Error("spelling out the default identity header or bind must not bounce the proxy")
	}

	// Broker/worker config is not the proxy's to care about.
	unrelated := &config.App{Remote: base.Remote, Workers: []config.Worker{{Name: "w", Dir: "d"}}}
	if RemoteConfigHash(unrelated) != h {
		t.Error("an unrelated config change must NOT bounce the proxy")
	}
}

// TestRemoteConfigHashCoversWhatTheProxyCaptures: the proxy reads more than the
// `remote:` block at startup — app.Name picks the JAIL it dials and app.Backend
// gates the transport. An independent review caught that hashing app.Remote
// alone let a rename leave a running proxy fronting the old machine's hub while
// apply reused it.
func TestRemoteConfigHashCoversWhatTheProxyCaptures(t *testing.T) {
	base := &config.App{Name: "assistant", Backend: "orbstack",
		Remote: config.Remote{Enabled: true, Port: 8445, BaseURL: "https://h.ts.net"}}
	h := RemoteConfigHash(base)

	renamed := *base
	renamed.Name = "other"
	if RemoteConfigHash(&renamed) == h {
		t.Error("a renamed instance must not reuse a proxy pointed at the old jail")
	}
	rebacked := *base
	rebacked.Backend = "lima"
	if RemoteConfigHash(&rebacked) == h {
		t.Error("a changed backend must not reuse a proxy built for the other transport")
	}
}

// TestRemoteIdentityMirrorsConfigRemote: RemoteConfigHash copies config.Remote
// onto state.RemoteIdentity field by field. A field added to config.Remote
// without a counterpart here would silently escape the hash, and a running
// proxy would keep the old value across apply. Name and Backend are the two
// App-level extras the proxy also captures.
func TestRemoteIdentityMirrorsConfigRemote(t *testing.T) {
	remote := reflect.TypeFor[config.Remote]()
	identity := reflect.TypeFor[state.RemoteIdentity]()
	for i := range remote.NumField() {
		f := remote.Field(i)
		g, ok := identity.FieldByName(f.Name)
		if !ok {
			t.Errorf("config.Remote.%s has no state.RemoteIdentity counterpart — add it to RemoteConfigHash", f.Name)
			continue
		}
		// allowed_users is hashed through remoteUserKeys (one string per
		// user, tier and agents included), not copied as its config type.
		if f.Name == "AllowedUsers" && g.Type == reflect.TypeFor[[]string]() {
			continue
		}
		// agent_messages is hashed as AgentMessagesOn: the proxy reads only
		// whether to filter, never the limits (the broker enforces those).
		if f.Name == "AgentMessages" && g.Type == reflect.TypeFor[bool]() {
			continue
		}
		// files is hashed as its effective limits and the agents'
		// workspaces, and only while on (FilesOn).
		if f.Name == "Files" && g.Type == reflect.TypeFor[*state.FilesIdentity]() {
			continue
		}
		// voice is hashed as its effective settings, and only while on
		// (VoiceOn).
		if f.Name == "Voice" && g.Type == reflect.TypeFor[*state.VoiceIdentity]() {
			continue
		}
		// push is hashed as its subject while on: the proxy captures only
		// whether to push and the VAPID subject.
		if f.Name == "Push" && g.Type == reflect.TypeFor[string]() {
			continue
		}
		if g.Type != f.Type {
			t.Errorf("state.RemoteIdentity.%s is %s, config.Remote.%s is %s", f.Name, g.Type, f.Name, f.Type)
		}
	}
	const appExtras = 4 // Name, Backend, Workers, Tree
	if got, want := identity.NumField(), remote.NumField()+appExtras; got != want {
		t.Errorf("state.RemoteIdentity has %d fields, want %d (config.Remote + Name + Backend + Workers + Tree)", got, want)
	}
}

// TestRemoteConfigHashCoversTiers: an operator entry hashes as its bare
// login (a pre-tier config keeps its hash), and a contact's tier or agents
// change the hash.
func TestRemoteConfigHashCoversTiers(t *testing.T) {
	if got := remoteUserKeys([]config.RemoteUser{{Login: "op@x"}}); got[0] != "op@x" {
		t.Fatalf("operator key %q, want the bare login", got[0])
	}
	a := remoteUserKeys([]config.RemoteUser{{Login: "c@x", Tier: config.TierContact, Agents: []string{"w1"}}})
	b := remoteUserKeys([]config.RemoteUser{{Login: "c@x", Tier: config.TierContact, Agents: []string{"w1", "w2"}}})
	if a[0] == b[0] || a[0] == "c@x" {
		t.Fatalf("contact keys %q / %q do not carry tier and agents", a[0], b[0])
	}
}

func remoteTestApp(t *testing.T) *config.App {
	t.Helper()
	return &config.App{Name: "boss", Tree: "/t", Workers: []config.Worker{{Name: "w1", Dir: "workers/w1"}, {Name: "w2", Dir: "workers/w2"}},
		Remote: config.Remote{Enabled: true, Port: 8445, BaseURL: "https://h.ts.net", Landing: config.RemoteLandingChat,
			AllowedUsers: []config.RemoteUser{{Login: "op@x"}, {Login: "c@x", Tier: config.TierContact, Agents: []string{"w1"}}}}}
}

func TestRemoteHashCoversChatListInputs(t *testing.T) {
	app := remoteTestApp(t)
	h0 := RemoteConfigHash(app)
	for name, mut := range map[string]func(*config.App){
		"see":         func(a *config.App) { a.Remote.AllowedUsers[1].See = []string{"w2"} },
		"labels_file": func(a *config.App) { a.Remote.LabelsFile = "workers/labels.json" },
		"workers":     func(a *config.App) { a.Workers = append(a.Workers, config.Worker{Name: "w9", Dir: "workers/w9"}) },
		"tree":        func(a *config.App) { a.Tree = "/other" },
	} {
		a := *app
		a.Workers = slices.Clone(app.Workers)
		a.Remote.AllowedUsers = slices.Clone(app.Remote.AllowedUsers)
		mut(&a)
		if RemoteConfigHash(&a) == h0 {
			t.Errorf("%s: hash unchanged; a running proxy would keep the old list", name)
		}
	}
}

func TestRemoteHashOfAConsoleInstanceIgnoresWorkers(t *testing.T) {
	app := remoteTestApp(t)
	app.Remote.Landing = ""
	h0 := RemoteConfigHash(app)
	app.Workers = append(app.Workers, config.Worker{Name: "w9", Dir: "workers/w9"})
	app.Tree = "/other"
	if RemoteConfigHash(app) != h0 {
		t.Fatal("a worker change bounced a console-landing proxy")
	}
}

func TestRemoteConfigHashCoversAgentMessages(t *testing.T) {
	app := &config.App{Name: "hello", Backend: "orbstack", Remote: config.Remote{Enabled: true, BaseURL: "https://x.ts.net",
		AllowedUsers: []config.RemoteUser{{Login: "op@x"}, {Login: "c@x", Tier: config.TierContact, Agents: []string{"w1"}}}}}
	off := RemoteConfigHash(app)
	app.Remote.AgentMessages.Enabled = true
	if RemoteConfigHash(app) == off {
		t.Fatal("the proxy filters only while on: turning it on must restart the proxy")
	}
	app.Remote.AgentMessages.Enabled = false
	app.Remote.AgentMessages.MaxChars = 100
	if RemoteConfigHash(app) != off {
		t.Fatal("off must keep the proxy's hash")
	}
}
func TestRemoteConfigHashPush(t *testing.T) {
	app := &config.App{Name: "hello", Backend: "orbstack", Tree: "/tmp/tree", Remote: config.Remote{Enabled: true,
		BaseURL: "https://mac.ts.net", Landing: config.RemoteLandingChat, AllowedUsers: []config.RemoteUser{{Login: "op@x"}}}}
	off := RemoteConfigHash(app)
	app.Remote.Push.Subject = "mailto:a@b" // set but off
	if RemoteConfigHash(app) != off {
		t.Fatal("a subject without enabled must not restart the proxy")
	}
	app.Remote.Push.Enabled = true
	on := RemoteConfigHash(app)
	if on == off {
		t.Fatal("turning push on must restart the proxy")
	}
	app.Remote.Push.Subject = "mailto:c@d"
	if RemoteConfigHash(app) == on {
		t.Fatal("the VAPID subject is captured at start: a change must restart the proxy")
	}
	subj := RemoteConfigHash(app)
	app.Remote.Push.TestHosts = []string{"127.0.0.1:9447"}
	if RemoteConfigHash(app) == subj {
		t.Fatal("the test hosts are captured at start: a change must restart the proxy")
	}
}
