package brokerctl

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/convbackup"
	"github.com/stevegeek/lever/internal/state"
)

// A backup younger than dailyBackupDue makes the daily backup a no-op: it
// returns no id and never reaches the guest (the runner here is nil, so a
// capture would fail).
func TestDailyBackupSkipsARecentBackup(t *testing.T) {
	root := t.TempDir()
	st := state.State{Dir: filepath.Join(root, ".lever-state")}
	app := &config.App{Tree: filepath.Join(root, "tree")}
	if err := os.MkdirAll(st.AgentConversations(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app.Tree, 0o755); err != nil {
		t.Fatal(err)
	}
	store := convbackup.Store{Dir: filepath.Join(st.AgentConversations(), "s-abc234")}
	if _, err := store.Save(convbackup.Meta{Agent: "s-abc234", Created: time.Now().Add(-2 * time.Hour).UTC(), Reason: "daily"},
		func(w io.Writer) (convbackup.Stats, error) { return convbackup.Stats{}, nil }); err != nil {
		t.Fatal(err)
	}
	id, err := agentDailyBackup(app, st, nil, "/workspace", "test")(context.Background(), "s-abc234")
	if err != nil || id != "" {
		t.Fatalf("id %q, %v", id, err)
	}
}

// gc removes one agent's backup directory and counts it; a dry run only
// counts; a name that is not an agent's, or a link in its place, is refused.
func TestRemoveAgentConversations(t *testing.T) {
	root := t.TempDir()
	st := state.State{Dir: filepath.Join(root, ".lever-state")}
	app := &config.App{Tree: filepath.Join(root, "tree")}
	dir := filepath.Join(st.AgentConversations(), "s-abc234", "20261124T120000Z")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "projects.tar.gz"), make([]byte, 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	rm := removeAgentConversations(app, st)
	if n, err := rm("s-abc234", true); err != nil || n != 1000 {
		t.Fatalf("dry run: %d %v", n, err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("a dry run removed it")
	}
	if n, err := rm("s-abc234", false); err != nil || n != 1000 {
		t.Fatalf("remove: %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(st.AgentConversations(), "s-abc234")); !os.IsNotExist(err) {
		t.Fatal("not removed")
	}
	if n, err := rm("s-abc234", false); err != nil || n != 0 {
		t.Fatalf("again: %d %v", n, err)
	}
	for _, bad := range []string{"..", "../x", "", "S-ABC"} {
		if _, err := rm(bad, true); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(st.AgentConversations(), "s-link11")); err != nil {
		t.Fatal(err)
	}
	if _, err := rm("s-link11", false); err == nil {
		t.Fatal("a link in place of the backup directory was followed")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("the link's target was removed")
	}
}
