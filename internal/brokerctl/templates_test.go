package brokerctl

import (
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/config"
)

// An instance's spec comes from its template only: its own dir under
// dir_root, its own ticket dir, the template's image and limits, and no
// shared folder.
func TestTemplateSpecs(t *testing.T) {
	app := &config.App{Name: "app", Tree: "/tree", Manager: config.Manager{Image: "lever.local/m:1"},
		Templates: []config.Template{{Name: "session", Prefix: "s", DirRoot: "sessions/", Image: "lever.local/s:1",
			Resources: config.Resources{Memory: "2GiB"}, MaxInstances: 3, MaxRunning: 2, Spawners: []string{"manager"},
			Obtain: []config.Grant{{Tool: "db", Op: "read"}}}}}
	specs := TemplateSpecs(app, "/lever", "1000", "v1")
	if len(specs) != 1 {
		t.Fatal(len(specs))
	}
	ts := specs[0]
	if ts.Prefix != "s" || ts.DirRoot != "sessions" || ts.OnDestroy != config.WorkspaceArchive || !ts.ManagerMay || len(ts.Obtain) != 1 || !strings.Contains(string(ts.Skill), "lever-version") {
		t.Fatalf("%+v", ts)
	}
	w := ts.Worker("s-abc234")
	if w.Name != "s-abc234" || w.WorkspaceSubdir != "sessions/s-abc234" || w.HostWorkspace != "/tree/sessions/s-abc234" ||
		!strings.HasSuffix(w.TicketDir, "/s-abc234") || !strings.Contains(w.Image, "s:1") || w.Resources.Memory == "" || w.Shared != nil || w.Recyclable {
		t.Fatalf("%+v", w)
	}
}

// A configured name that a spawned agent in the registry used refuses the
// broker's start.
func TestCheckRegistryNames(t *testing.T) {
	app := &config.App{Name: "app", Workers: []config.Worker{{Name: "s-abc234"}}}
	st := broker.AgentsState{Agents: []broker.AgentRecord{{Name: "s-abc234", State: broker.AgentDestroyed}}}
	if err := checkRegistryNames(app, st); err == nil {
		t.Fatal("collision allowed")
	}
	if err := checkRegistryNames(&config.App{Name: "app"}, st); err != nil {
		t.Fatal(err)
	}
}
