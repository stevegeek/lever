package brokerctl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

func chatApp(tree string, remote bool, users ...string) *config.App {
	return &config.App{Tree: tree, Remote: config.Remote{Enabled: remote, AllowedUsers: remoteUsers(users...)}}
}

func TestChatLedgerPath(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "workspace")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	st := state.ForConfig(root)
	cases := []struct {
		name string
		app  *config.App
		st   state.State
		want string
	}{
		{"remote on with allowed_users", chatApp(tree, true, "op@example.com"), st, st.ChatLedger()},
		{"remote off", chatApp(tree, false, "op@example.com"), st, ""},
		{"no allowed_users", chatApp(tree, true), st, ""},
		{"state dir inside the tree", chatApp(root, true, "op@example.com"), st, ""},
		{"state dir is the tree", chatApp(st.Dir, true, "op@example.com"), st, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChatLedgerPath(tc.app, tc.st); got != tc.want {
				t.Fatalf("ChatLedgerPath = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStateInsideTreeSeesThroughSymlinks: a tree reached through a symlink
// that contains the state directory is still "inside".
func TestStateInsideTreeSeesThroughSymlinks(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "tree-link")
	if err := os.Symlink(root, link); err != nil {
		t.Skip(err)
	}
	st := state.ForConfig(root)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !StateInsideTree(chatApp(link, true, "op"), st) {
		t.Fatal("StateInsideTree missed a tree reached through a symlink")
	}
}

// TestConfigHashCoversVerifiedChat: turning allowed_users on or off changes
// the broker's hash, so apply restarts the broker with the new setting.
func TestConfigHashCoversVerifiedChat(t *testing.T) {
	on := chatApp("/t", true, "op@example.com")
	off := chatApp("/t", true)
	if ConfigHash(on) == ConfigHash(off) {
		t.Fatal("ConfigHash does not change when verified chat is turned on")
	}
}

func remoteUsers(logins ...string) []config.RemoteUser {
	out := make([]config.RemoteUser, len(logins))
	for i, l := range logins {
		out[i] = config.RemoteUser{Login: l}
	}
	return out
}
