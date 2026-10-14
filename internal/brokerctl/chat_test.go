package brokerctl

import (
	"os"
	"path/filepath"
	"strings"
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

// TestStateInsideTreeByIdentity: state directories that a comparison of
// resolved paths places outside the tree, though they are (or would be
// created) inside it.
func TestStateInsideTreeByIdentity(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	inst := filepath.Join(base, "inst")
	for _, d := range []string{filepath.Join(tree, "deep"), inst} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(tree, filepath.Join(inst, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tree, "not-yet"), filepath.Join(inst, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tree, "deep"), filepath.Join(inst, "deep")); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"missing below a link into the tree": filepath.Join(inst, "link", "x", ".lever-state"),
		"a dangling link into the tree":      filepath.Join(inst, "dangling"),
		"a .. after a link into the tree":    inst + "/deep/../.lever-state",
	} {
		t.Run(name, func(t *testing.T) {
			st := state.State{Dir: dir}
			app := chatApp(tree, true, "op")
			if inside, err := StateInsideTreeErr(app, st); !inside || err != nil {
				t.Fatalf("StateInsideTreeErr = %v, %v; want inside", inside, err)
			}
			if ChatLedgerPath(app, st) != "" {
				t.Fatal("a chat ledger inside the tree")
			}
		})
	}
	st := state.State{Dir: filepath.Join(inst, ".lever-state")}
	if inside, err := StateInsideTreeErr(chatApp(tree, true, "op"), st); inside || err != nil {
		t.Fatalf("StateInsideTreeErr = %v, %v; want outside", inside, err)
	}
}

// TestStateInsideTreeFailsClosed: a state directory lever cannot place is
// inside, and the error says why.
func TestStateInsideTreeFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	locked := filepath.Join(base, "locked")
	for _, d := range []string{tree, filepath.Join(locked, ".lever-state")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	st := state.State{Dir: filepath.Join(locked, ".lever-state")}
	app := chatApp(tree, true, "op")
	inside, err := StateInsideTreeErr(app, st)
	if !inside || err == nil || !strings.Contains(err.Error(), "cannot prove") {
		t.Fatalf("StateInsideTreeErr = %v, %v; want inside with an error", inside, err)
	}
	if !StateInsideTree(app, st) {
		t.Fatal("StateInsideTree must fail closed")
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
