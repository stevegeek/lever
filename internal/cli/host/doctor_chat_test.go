package host

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

func verifiedChatFixture(t *testing.T, users ...string) (*config.App, state.State) {
	t.Helper()
	root := t.TempDir()
	tree := filepath.Join(root, "workspace")
	st := state.ForConfig(root)
	for _, d := range []string{tree, st.Dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &config.App{Tree: tree, Remote: config.Remote{Enabled: true, AllowedUsers: remoteUsers(users...)}}, st
}

func TestCheckVerifiedChat(t *testing.T) {
	t.Run("remote off", func(t *testing.T) {
		app, st := verifiedChatFixture(t, "op@example.com")
		app.Remote.Enabled = false
		if r := checkVerifiedChat(app, st); !r.ok || r.fix != "" {
			t.Fatalf("row = %+v, want a plain pass", r)
		}
	})
	t.Run("no allowed_users warns", func(t *testing.T) {
		app, st := verifiedChatFixture(t)
		if r := checkVerifiedChat(app, st); !r.ok || r.fix == "" || !strings.Contains(r.detail, "allowed_users") {
			t.Fatalf("row = %+v, want a warning naming allowed_users", r)
		}
	})
	t.Run("state inside the tree fails", func(t *testing.T) {
		app, st := verifiedChatFixture(t, "op@example.com")
		app.Tree = filepath.Dir(st.Dir)
		if r := checkVerifiedChat(app, st); r.ok {
			t.Fatalf("row = %+v, want a failure", r)
		}
	})
	t.Run("on, nothing recorded yet", func(t *testing.T) {
		app, st := verifiedChatFixture(t, "op@example.com")
		if r := checkVerifiedChat(app, st); !r.ok || r.fix != "" || !strings.Contains(r.detail, "no chat post") {
			t.Fatalf("row = %+v", r)
		}
	})
	t.Run("on, ledger 0700", func(t *testing.T) {
		app, st := verifiedChatFixture(t, "op@example.com")
		if err := os.Mkdir(st.ChatLedger(), 0o700); err != nil {
			t.Fatal(err)
		}
		if r := checkVerifiedChat(app, st); !r.ok || r.fix != "" {
			t.Fatalf("row = %+v, want a pass", r)
		}
	})
	t.Run("writable ledger fails", func(t *testing.T) {
		app, st := verifiedChatFixture(t, "op@example.com")
		if err := os.Mkdir(st.ChatLedger(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(st.ChatLedger(), 0o777); err != nil {
			t.Fatal(err)
		}
		if r := checkVerifiedChat(app, st); r.ok {
			t.Fatalf("row = %+v, want a failure", r)
		}
	})
}

func remoteUsers(logins ...string) []config.RemoteUser {
	out := make([]config.RemoteUser, len(logins))
	for i, l := range logins {
		out[i] = config.RemoteUser{Login: l}
	}
	return out
}

func TestCheckChatLabels(t *testing.T) {
	tree := t.TempDir()
	app := &config.App{Name: "boss", Tree: tree, Workers: []config.Worker{{Name: "w1"}},
		Remote: config.Remote{Enabled: true, LabelsFile: "labels.json"}}
	p := filepath.Join(tree, "labels.json")
	write := func(b []byte) func() {
		return func() {
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, tc := range map[string]struct {
		setup    func()
		warn     bool
		contains string
	}{
		"absent":    {func() {}, false, "absent"},
		"good":      {write([]byte(`{"w1":"a","boss":"b","zz":"c"}`)), false, "2 label(s) for known agents in labels.json (1 other name(s) ignored)"},
		"not json":  {write([]byte(`[1]`)), true, "is not a JSON object of strings"},
		"too big":   {write(bytes.Repeat([]byte(" "), 16<<10+1)), true, "is larger than 16 KiB"},
		"symlink":   {func() { _ = os.Symlink("/etc/hosts", p) }, true, "symbolic link"},
		"directory": {func() { _ = os.Mkdir(p, 0o755) }, true, "is not a regular file"},
	} {
		t.Run(name, func(t *testing.T) {
			_ = os.RemoveAll(p)
			tc.setup()
			r := checkChatLabels(app)
			// A bad file is a warning: a passing row that carries a fix.
			if !r.ok || (r.fix != "") != tc.warn || !strings.Contains(r.detail, tc.contains) || r.name != "chat labels" {
				t.Fatalf("%+v", r)
			}
		})
	}
	app.Remote.LabelsFile = ""
	if r := checkChatLabels(app); !r.ok || !strings.Contains(r.detail, "not set") {
		t.Fatalf("unset: %+v", r)
	}
}

// The row names each contact's reach: the agents it messages and those it
// may only see.
func TestCheckVerifiedChatNamesSeeLists(t *testing.T) {
	app, st := verifiedChatFixture(t, "op@example.com")
	app.Remote.AllowedUsers = append(app.Remote.AllowedUsers,
		config.RemoteUser{Login: "c@x", Tier: config.TierContact, Agents: []string{"w1", "w3"}, See: []string{"w2"}},
		config.RemoteUser{Login: "d@x", Tier: config.TierContact, Agents: []string{"w4"}})
	r := checkVerifiedChat(app, st)
	if !strings.Contains(r.detail, "c@x contact (w1, w3; see: w2)") || !strings.Contains(r.detail, "d@x contact (w4)") {
		t.Fatalf("row = %+v", r)
	}
}

func TestCheckAgentMessages(t *testing.T) {
	tree := t.TempDir()
	st := state.State{Dir: t.TempDir()}
	app := &config.App{Name: "x", Tree: tree, Remote: config.Remote{Enabled: true,
		AllowedUsers: []config.RemoteUser{{Login: "op@x"}, {Login: "c@x", Tier: config.TierContact, Agents: []string{"x"}}}}}
	if r := checkAgentMessages(app, st); !r.ok || !strings.Contains(r.detail, "off") {
		t.Fatalf("off: %+v", r)
	}
	app.Remote.AgentMessages.Enabled = true
	r := checkAgentMessages(app, st)
	if !r.ok || !strings.Contains(r.detail, "c@x (x)") || !strings.Contains(r.detail, "follow_up_after 24h,") ||
		!strings.Contains(r.detail, "max_chars 4000") || !strings.Contains(r.detail, "image") {
		t.Fatalf("on: %+v", r)
	}
	if strings.Contains(r.detail, "op@x") {
		t.Fatalf("an operator is not a contact: %+v", r)
	}
	app.Remote.AgentMessages.FollowUpAfter = 90 * time.Minute
	if r := checkAgentMessages(app, st); !strings.Contains(r.detail, "follow_up_after 1h30m0s") {
		t.Fatalf("a duration that is not whole hours: %+v", r)
	}
	if err := os.MkdirAll(st.AgentLedger(), 0o700); err != nil {
		t.Fatal(err)
	}
	if r := checkAgentMessages(app, st); !r.ok || !strings.Contains(r.detail, "0700") {
		t.Fatalf("a private ledger: %+v", r)
	}
	if err := os.Chmod(st.AgentLedger(), 0o777); err != nil {
		t.Fatal(err)
	}
	if r := checkAgentMessages(app, st); r.ok {
		t.Fatalf("a group-writable ledger must fail: %+v", r)
	}
	inside := state.State{Dir: filepath.Join(tree, ".lever-state")}
	if r := checkAgentMessages(app, inside); r.ok {
		t.Fatalf("state inside the tree must fail: %+v", r)
	}
}
