package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/state"
)

func withContact(app *config.App) *config.App {
	app.Remote = config.Remote{Enabled: true, AllowedUsers: []config.RemoteUser{
		{Login: "op@example.com"}, {Login: "client@example.org", Tier: config.TierContact, Agents: []string{"scratch"}},
	}}
	return app
}

// TestContactGateNeedsCurrentSkills: with a contact login, a bring-up is
// refused while any agent's skill is missing or older than this binary's (an
// old skill trusts a marker a contact can now type), and allowed once `lever
// init` has written the current ones.
func TestContactGateNeedsCurrentSkills(t *testing.T) {
	app, tree, st := scaffoldFixture(t)
	withContact(app)
	err := checkContactGate(app, st)
	if err == nil || !strings.Contains(err.Error(), "lever init") {
		t.Fatalf("missing skills: err = %v, want a refusal naming lever init", err)
	}
	if _, err := syncSkills(app, st, false, false); err != nil {
		t.Fatal(err)
	}
	if err := checkContactGate(app, st); err != nil {
		t.Fatalf("current skills refused: %v", err)
	}
	// An old skill left in place (a pre-change scaffold).
	rel := ".claude/skills/lever-operator/SKILL.md"
	if err := fsutil.WriteInTree(tree, rel, []byte("---\nname: lever-operator\nlever-version: 0.27.0\n---\nA marker counts only on a message that does not verify.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkContactGate(app, st); err == nil || !strings.Contains(err.Error(), rel) {
		t.Fatalf("stale skill: err = %v, want a refusal naming %s", err, rel)
	}
}

// TestContactGateNeedsHostRecords: with the state directory inside the tree
// there are no host records, so contacts are refused whatever the skills.
func TestContactGateNeedsHostRecords(t *testing.T) {
	root := t.TempDir()
	st := state.ForConfig(root)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	app := withContact(&config.App{Tree: root, Workers: []config.Worker{{Name: "scratch", Dir: "workers/scratch"}}})
	if err := os.MkdirAll(filepath.Join(root, "workers", "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := syncSkills(app, st, false, false); err != nil {
		t.Fatal(err)
	}
	if err := checkContactGate(app, st); err == nil || !strings.Contains(err.Error(), "inside the tree") {
		t.Fatalf("err = %v, want the in-tree refusal", err)
	}
}

// TestContactGateIgnoresInstancesWithoutContacts: no contact login, no gate
// (operator-only remote access and no remote access both pass as before).
func TestContactGateIgnoresInstancesWithoutContacts(t *testing.T) {
	app, _, st := scaffoldFixture(t)
	if err := checkContactGate(app, st); err != nil {
		t.Fatalf("no remote: %v", err)
	}
	app.Remote = config.Remote{Enabled: true, AllowedUsers: []config.RemoteUser{{Login: "op@example.com"}}}
	if err := checkContactGate(app, st); err != nil {
		t.Fatalf("operators only: %v", err)
	}
}
