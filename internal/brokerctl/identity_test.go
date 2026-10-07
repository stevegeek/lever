package brokerctl

import (
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

// ConfigHash must be deterministic, sensitive to broker-relevant config
// (tools, worker specs), and INSENSITIVE to config the broker does not bake
// in at start (a manager-image change must not bounce the broker).
func TestConfigHash(t *testing.T) {
	mk := func() *config.App {
		return &config.App{
			Name: "hello", Backend: "orbstack", Tree: "/tmp/tree",
			Manager: config.Manager{Image: "img"},
			Broker: config.Broker{
				JailPort: 8443, AdminPort: 8444,
				Tools: []config.Tool{{Name: "db", Command: []string{"db-server"}}},
			},
			Workers: []config.Worker{{Name: "scratch", Dir: "workers/scratch"}},
		}
	}

	base := mk()
	if got, again := ConfigHash(base), ConfigHash(mk()); got == "" || got != again {
		t.Fatalf("hash not deterministic: %q vs %q", got, again)
	}

	tool := mk()
	tool.Broker.Tools = append(tool.Broker.Tools, config.Tool{Name: "qmd", Command: []string{"qmd-server"}})
	if ConfigHash(tool) == ConfigHash(base) {
		t.Fatal("adding a tool must change the hash")
	}

	worker := mk()
	worker.Workers[0].Dir = "workers/elsewhere"
	if ConfigHash(worker) == ConfigHash(base) {
		t.Fatal("changing a worker spec must change the hash")
	}

	image := mk()
	image.Manager.Image = "img:v2"
	if ConfigHash(image) != ConfigHash(base) {
		t.Fatal("a manager-image change must NOT change the hash (no broker restart)")
	}

	// The manager's claude block rides its envelope: a change must restart
	// the broker so the next apply re-mints and re-stages it.
	claude := mk()
	claude.Manager.ClaudeSettings.AutoCompactWindow = 400000
	if ConfigHash(claude) == ConfigHash(base) {
		t.Fatal("changing manager.claude_settings must change the hash")
	}
	note := mk()
	note.Manager.AfterCompactNote = "Re-read NOTES.md"
	if ConfigHash(note) == ConfigHash(base) || ConfigHash(note) == ConfigHash(claude) {
		t.Fatal("changing manager.after_compact_note must change the hash")
	}
	workerNote := mk()
	workerNote.Workers[0].AfterCompactNote = "Re-read TASK.md"
	if ConfigHash(workerNote) == ConfigHash(base) {
		t.Fatal("changing a worker's after_compact_note must change the hash")
	}
}

// A config that sets none of the claude keys hashes exactly as on 673beaa
// (v0.33.1, before they existed), so upgrading lever does not restart the
// broker for them. The golden value was computed on that commit.
func TestConfigHashUnchangedWithoutClaudeKeys(t *testing.T) {
	app := &config.App{
		Name: "hello", Backend: "orbstack", Tree: "/tmp/tree",
		Manager: config.Manager{Image: "img"},
		Broker: config.Broker{
			JailPort: 8443, AdminPort: 8444,
			Tools: []config.Tool{{Name: "db", Command: []string{"db-server"}}},
		},
		Workers: []config.Worker{{Name: "scratch", Dir: "workers/scratch", Model: "m", InstructionsFile: "w.md"}},
	}
	const golden = "52dcb0680131c2514bd5fb09896b8ca1241469397b55f1af30011323c17112ce"
	if got := ConfigHash(app); got != golden {
		t.Fatalf("ConfigHash = %s, want the v0.33.1 hash %s (an unset new field must not change it)", got, golden)
	}
}

// TestConfigHashFollowsTheRemoteLogins: the broker routes a message by its
// sender label, so replacing one allowed login with another (same count, same
// on/off) must restart it.
func TestConfigHashFollowsTheRemoteLogins(t *testing.T) {
	mk := func(logins ...string) *config.App {
		app := &config.App{Name: "hello", Backend: "orbstack", Tree: "/tmp/tree", Remote: config.Remote{Enabled: true}}
		for _, l := range logins {
			app.Remote.AllowedUsers = append(app.Remote.AllowedUsers, config.RemoteUser{Login: l})
		}
		return app
	}
	if ConfigHash(mk("a@example.com")) == ConfigHash(mk("b@example.com")) {
		t.Fatal("changing an allowed login must change the hash")
	}
	if ConfigHash(mk("a@example.com")) != ConfigHash(mk("a@example.com")) {
		t.Fatal("hash not deterministic")
	}
	if got := WebSenders(mk("B@Example.com", "usr_1")); len(got) != 2 || got[0] != "user:b@example.com" || got[1] != "user:usr_1@id.lever.local" {
		t.Fatalf("WebSenders = %v", got)
	}
	if got := WebSenders(mk()); len(got) != 1 || got[0] != "user:lever-operator@lever.local" {
		t.Fatalf("WebSenders with no allowed_users = %v, want the unnamed operator", got)
	}
	off := mk("a@example.com")
	off.Remote.Enabled = false
	if got := WebSenders(off); got != nil {
		t.Fatalf("WebSenders with remote off = %v", got)
	}
}

// Off, the broker's stamp is the one it had before agent_messages existed:
// an upgrade alone never bounces the broker.
func TestConfigHashUnchangedWhileAgentMessagesOff(t *testing.T) {
	app := &config.App{Name: "hello", Backend: "orbstack", Tree: "/tmp/tree", Remote: config.Remote{Enabled: true,
		AllowedUsers: []config.RemoteUser{{Login: "op@x"}, {Login: "c@x", Tier: config.TierContact, Agents: []string{"w1"}}}}}
	old := state.HashJSON(struct {
		Broker       config.Broker
		Workers      []config.Worker
		Scion        config.ScionConfig
		VerifiedChat bool
		WebSenders   []string `json:",omitempty"`
	}{app.Broker, app.Workers, app.Scion, ChatConfigured(app), WebSenders(app)})
	if ConfigHash(app) != old {
		t.Fatal("off must keep the pre-change hash")
	}
	app.Remote.AgentMessages = config.AgentMessages{FollowUpAfter: 48 * time.Hour} // set but off
	if ConfigHash(app) != old {
		t.Fatal("limits without enabled must not change the hash")
	}
	app.Remote.AgentMessages.Enabled = true
	on := ConfigHash(app)
	if on == old {
		t.Fatal("turning agent messages on must bounce the broker")
	}
	app.Remote.AllowedUsers[1].Agents = []string{"w1", "w2"}
	if ConfigHash(app) == on {
		t.Fatal("a contact's agents must be in the stamp while on")
	}
}

func TestConfigHashFilesOnlyWhileOn(t *testing.T) {
	app := remoteTestApp(t)
	off := ConfigHash(app)
	app.Remote.Files.MaxBytes = 5 << 20 // set but off
	if ConfigHash(app) != off {
		t.Fatal("files settings while off changed the broker hash")
	}
	app.Remote.Files.Enabled = true
	on := ConfigHash(app)
	if on == off {
		t.Fatal("turning files on must bounce the broker")
	}
	app.Remote.Files.Extensions = []string{"pdf"}
	if ConfigHash(app) == on {
		t.Fatal("extensions must be in the broker hash")
	}
}

func TestRemoteConfigHashFilesOnlyWhileOn(t *testing.T) {
	app := remoteTestApp(t)
	off := RemoteConfigHash(app)
	app.Remote.Files.MaxBytes = 5 << 20
	if RemoteConfigHash(app) != off {
		t.Fatal("files settings while off changed the proxy hash")
	}
	app.Remote.Files.Enabled = true
	if RemoteConfigHash(app) == off {
		t.Fatal("turning files on must restart the proxy")
	}
}

// Off, the proxy's stamp is the one it had before remote.files existed.
func TestRemoteConfigHashUnchangedWhileFilesOff(t *testing.T) {
	app := remoteTestApp(t)
	id := state.RemoteIdentity{Enabled: true, Port: 8445, BaseURL: "https://h.ts.net", AllowedUsers: remoteUserKeys(app.Remote.AllowedUsers),
		IdentityHeader: app.EffectiveRemoteIdentityHeader(), Bind: app.EffectiveRemoteBind(), Landing: app.EffectiveRemoteLanding(),
		Name: "boss", Workers: []string{"w1", "w2"}, Tree: "/t"}
	if RemoteConfigHash(app) != state.RemoteConfigHash(id) {
		t.Fatal("off must keep the pre-change proxy hash")
	}
}

// The per-direction and per-login switches change both stamps only when
// set to their non-default value: an instance with files on and neither
// key keeps the stamps it had.
func TestFilesSwitchesInTheStamps(t *testing.T) {
	app := remoteTestApp(t)
	app.Remote.Files.Enabled = true
	oldBroker := state.HashJSON(struct {
		Broker        config.Broker
		Workers       []config.Worker
		Scion         config.ScionConfig
		VerifiedChat  bool
		WebSenders    []string            `json:",omitempty"`
		AgentMessages *agentMessagesStamp `json:",omitempty"`
		Files         *struct {
			MaxBytes   int64
			Extensions []string
			Contacts   []string
			Operators  []string
		} `json:",omitempty"`
	}{app.Broker, app.Workers, app.Scion, ChatConfigured(app), WebSenders(app), nil, &struct {
		MaxBytes   int64
		Extensions []string
		Contacts   []string
		Operators  []string
	}{app.EffectiveFilesMaxBytes(), app.EffectiveFilesExtensions(), []string{"c@x=w1"}, []string{"op@x"}}})
	if ConfigHash(app) != oldBroker {
		t.Fatal("defaults changed the broker stamp")
	}
	b0, p0 := ConfigHash(app), RemoteConfigHash(app)
	yes, no := true, false
	app.Remote.Files.Uploads, app.Remote.Files.Shares = &yes, &yes
	app.Remote.AllowedUsers[1].Files = &yes
	if ConfigHash(app) != b0 || RemoteConfigHash(app) != p0 {
		t.Fatal("explicit defaults changed a stamp")
	}
	app.Remote.Files.Uploads = &no
	if ConfigHash(app) != b0 || RemoteConfigHash(app) == p0 {
		t.Fatal("uploads off: the proxy restarts, the broker does not act on it")
	}
	app.Remote.Files.Uploads = &yes
	app.Remote.Files.Shares = &no
	if ConfigHash(app) == b0 || RemoteConfigHash(app) == p0 {
		t.Fatal("shares off must restart both")
	}
	app.Remote.Files.Shares = &yes
	app.Remote.AllowedUsers[1].Files = &no
	if ConfigHash(app) == b0 || RemoteConfigHash(app) == p0 {
		t.Fatal("a login with files: false must restart both")
	}
	app.Remote.Files.Enabled = false
	off := RemoteConfigHash(app)
	app.Remote.AllowedUsers[1].Files = nil
	if RemoteConfigHash(app) != off {
		t.Fatal("files off: the login switch is ignored")
	}
}
