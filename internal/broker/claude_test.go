package broker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/wire"
)

// A worker's envelope carries its own claude block, staged before every start
// and resume, so boot re-applies the host config each time.
func TestWorkerTicketCarriesClaude(t *testing.T) {
	want := &wire.Claude{AutoCompactWindow: 300000, AfterCompactNote: "Re-read TASK.md"}
	spec := WorkerSpec{Name: "worker", WorkspaceSubdir: "workers/worker",
		TicketDir: "/run/user/501/lever/tickets/worker", Claude: want}
	rt := &fakeRuntime{}
	b := newTestBroker(t, rt, spec)
	if err := b.stageWorkerTicket(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if got := rt.lastStaged(t, "worker").Claude; got == nil || *got != *want {
		t.Fatalf("staged claude = %+v, want %+v", got, want)
	}
	spec.Claude = nil
	if err := b.stageWorkerTicket(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if got := rt.lastStaged(t, "worker").Claude; got != nil {
		t.Fatalf("staged claude = %+v for a worker with none", got)
	}
}

// The healer's re-stage of the manager's ticket keeps the manager's block.
func TestManagerFreshTicketCarriesClaude(t *testing.T) {
	want := &wire.Claude{AutoCompactWindow: 400000}
	dir := filepath.Join(t.TempDir(), ".lever")
	b := New(testConfig(t, withManager("test-manager", "appname"),
		func(c *Config) { c.Identity.ManagerClaude = want }))
	if err := b.stageFreshTicket("test-manager", dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "bootstrap.json"))
	if err != nil {
		t.Fatal(err)
	}
	var bs wire.Bootstrap
	if err := json.Unmarshal(raw, &bs); err != nil {
		t.Fatal(err)
	}
	if bs.Claude == nil || *bs.Claude != *want {
		t.Fatalf("staged claude = %+v, want %+v", bs.Claude, want)
	}
}
