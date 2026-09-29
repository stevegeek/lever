package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
