package brokerctl

import (
	"testing"

	"github.com/stevegeek/lever/internal/config"
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
