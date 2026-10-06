package host

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
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

// TestContactSessionNeedsAFreshStartWithTheCurrentSkill: the proxy lets a
// contact post to an agent only when lever recorded a fresh start of that
// agent's session with the skill that is on disk now, and that skill is this
// version's. A session lever never recorded (an older lever created it), one
// that started before `lever init` rewrote the skill (a resumed 0.27
// conversation), and a stale skill on disk are all refused.
func TestContactSessionNeedsAFreshStartWithTheCurrentSkill(t *testing.T) {
	app, tree, st := scaffoldFixture(t)
	app.Name = "hello"
	withContact(app)
	rel := "workers/scratch/.claude/skills/lever-agent/SKILL.md"
	// A 0.27 skill on disk, and the worker's session started with it.
	old := []byte("---\nname: lever-agent\nlever-version: 0.27.0\n---\nOnly the FIRST line counts.\n")
	if err := fsutil.WriteInTree(tree, rel, old, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := contactSession(app, st, "scratch"); err == nil || !strings.Contains(err.Error(), "not lever") {
		t.Fatalf("stale skill on disk: %v", err)
	}
	if err := brokerctl.BeginSession(app, st, "0.27.0", "scratch")(); err != nil {
		t.Fatal(err)
	}
	// The operator upgrades and runs lever init: the disk is current, the
	// resumed session still holds the old text.
	if _, err := syncSkills(app, st, true, false); err != nil {
		t.Fatal(err)
	}
	if err := checkContactGate(app, st); err != nil {
		t.Fatalf("the disk gate: %v", err)
	}
	if err := contactSession(app, st, "scratch"); err == nil || !strings.Contains(err.Error(), "started before its current skill") {
		t.Fatalf("resumed old session: %v", err)
	}
	// An agent lever has no record of starting (the manager here).
	if err := contactSession(app, st, "hello"); err == nil || !strings.Contains(err.Error(), "no record") {
		t.Fatalf("unrecorded session: %v", err)
	}
	// A fresh start with the current skill is allowed.
	if err := brokerctl.BeginSession(app, st, cli.VersionString(), "scratch")(); err != nil {
		t.Fatal(err)
	}
	if err := contactSession(app, st, "scratch"); err != nil {
		t.Fatalf("fresh session refused: %v", err)
	}
	// An agent the instance does not have is refused.
	if err := contactSession(app, st, "nobody"); err == nil {
		t.Fatal("an unknown agent was allowed")
	}
}

// TestContactSessionRefusesInTreeState: with the state directory inside the
// tree the session record is agent-writable, so every post is refused.
func TestContactSessionRefusesInTreeState(t *testing.T) {
	root := t.TempDir()
	st := state.ForConfig(root)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	app := withContact(&config.App{Name: "hello", Tree: root, Workers: []config.Worker{{Name: "scratch", Dir: "workers/scratch"}}})
	if err := contactSession(app, st, "scratch"); err == nil || !strings.Contains(err.Error(), "inside the tree") {
		t.Fatalf("err = %v, want the in-tree refusal", err)
	}
	if err := brokerctl.BeginSession(app, st, "x", "scratch")(); err == nil {
		t.Fatal("a session was recorded inside the tree")
	}
}

// TestContactSessionWarningsNameTheAgentAndTheFix: a bring-up warns about
// each contact-listed agent whose session would be refused, with its fix.
func TestContactSessionWarningsNameTheAgentAndTheFix(t *testing.T) {
	app, _, st := scaffoldFixture(t)
	app.Name = "hello"
	withContact(app)
	app.Remote.AllowedUsers[1].Agents = []string{"scratch", "hello"}
	if _, err := syncSkills(app, st, false, false); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&out)
	printContactSessionWarnings(cmd, app, st)
	// One line for both agents, naming each and both fixes.
	if n := strings.Count(out.String(), "\n"); n != 1 {
		t.Fatalf("warnings %q: %d lines, want one summarised line", out.String(), n)
	}
	for _, want := range []string{"contacts cannot post to 2 agents", "scratch, hello (", "lever up --fresh", "a worker takes contact messages"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("warnings %q, want %q", out.String(), want)
		}
	}
	if err := brokerctl.BeginSession(app, st, cli.VersionString(), "scratch")(); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	printContactSessionWarnings(cmd, app, st)
	if strings.Contains(out.String(), "scratch") || !strings.Contains(out.String(), "contacts cannot post to hello (") || strings.Contains(out.String(), "a worker takes") {
		t.Fatalf("a fresh session still warned, or the manager's line is wrong: %q", out.String())
	}
}

// TestStaleSkillWarningWithVerifiedChat: with verified chat on and no
// contact, a bring-up warns about stale skills (a 0.27 manager skill reads
// the manager's own notes as data), and says nothing once they are current.
func TestStaleSkillWarningWithVerifiedChat(t *testing.T) {
	app, _, st := scaffoldFixture(t)
	app.Name = "hello"
	app.Remote = config.Remote{Enabled: true, AllowedUsers: []config.RemoteUser{{Login: "op@example.com"}}}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&out)
	printContactSessionWarnings(cmd, app, st)
	if !strings.Contains(out.String(), "lever init") || !strings.Contains(out.String(), "notes to itself") {
		t.Fatalf("stale skills: %q", out.String())
	}
	if _, err := syncSkills(app, st, false, false); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	printContactSessionWarnings(cmd, app, st)
	if out.Len() != 0 {
		t.Fatalf("current skills warned: %q", out.String())
	}
}

// TestContactSessionWarningIsBounded: many blocked agents give one line
// listing at most maxWarnedContactAgents names, grouped by reason, with the
// rest counted; names are sanitized for the terminal.
func TestContactSessionWarningIsBounded(t *testing.T) {
	blocked := []blockedContactAgent{{"evil\x1b]0;pwned\x07", "x"}}
	for i := range 11 {
		reason := "lever has no record of its session starting fresh"
		if i == 1 {
			reason = "its lever skill is not current"
		}
		blocked = append(blocked, blockedContactAgent{fmt.Sprintf("w%d", i), reason})
	}
	line := contactSessionWarning("hello", blocked)
	if strings.Contains(line, "\n") || strings.Contains(line, "\x1b") {
		t.Fatalf("line %q must be one line with no escape", line)
	}
	for _, want := range []string{"cannot post to 12 agents", "evil (x); w0, w2, w3, w4, w5, w6 (lever has no record", "w1 (its lever skill is not current)", "and 4 more"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %q, want %q", line, want)
		}
	}
	if strings.Contains(line, "w7") || strings.Contains(line, "lever up --fresh") {
		t.Fatalf("line %q lists past the bound or names a manager fix with no manager blocked", line)
	}
	if contactSessionWarning("hello", nil) != "" {
		t.Fatal("nothing blocked must print nothing")
	}
}
