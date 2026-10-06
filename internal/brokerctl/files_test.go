package brokerctl

import (
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

func TestFilesConfigFromApp(t *testing.T) {
	app := &config.App{Name: "hello", Tree: t.TempDir(), Workers: []config.Worker{{Name: "w1", Dir: "workers/w1"}},
		Remote: config.Remote{Enabled: true, Landing: config.RemoteLandingChat,
			AllowedUsers: []config.RemoteUser{{Login: "op@x"}, {Login: "c@x", Tier: config.TierContact, Agents: []string{"w1"}, See: []string{"hello"}}}}}
	st := state.State{Dir: t.TempDir()}
	if c := Files(app, st); c.Enabled {
		t.Fatal("off by default")
	}
	app.Remote.Files.Enabled = true
	c := Files(app, st)
	if !c.Enabled || c.Tree != app.Tree || c.Workspaces["hello"] != "." || c.Workspaces["w1"] != "workers/w1" || c.LedgerDir != st.FilesLedger() ||
		len(c.Contacts) != 1 || c.Contacts[0].Login != "c@x" || len(c.Contacts[0].Agents) != 1 ||
		len(c.Operators) != 1 || c.Operators[0] != "op@x" || c.MaxBytes != 25<<20 || len(c.Extensions) == 0 {
		t.Fatalf("%+v", c)
	}
	if Files(app, state.State{Dir: filepath.Join(app.Tree, ".lever-state")}).LedgerDir != "" {
		t.Fatal("a state directory inside the tree must leave the ledger off")
	}
	app.Remote.Landing = ""
	if Files(app, st).Enabled {
		t.Fatal("files need landing chat")
	}
}
