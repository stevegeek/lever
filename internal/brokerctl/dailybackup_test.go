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
