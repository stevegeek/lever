package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/skills"
)

func scaffoldFixture(t *testing.T) (*config.App, string, string) {
	t.Helper()
	root := t.TempDir()
	tree := filepath.Join(root, "workspace")
	for _, d := range []string{filepath.Join(tree, "workers", "scratch"), filepath.Join(root, ".lever-state")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	app := &config.App{Tree: tree, Workers: []config.Worker{{Name: "scratch", Dir: "workers/scratch"}}}
	return app, tree, filepath.Join(root, ".lever-state")
}

func readState(t *testing.T, stateDir string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(stateDir, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSyncSkillsFreshCreatesAllAndRecordsHashes(t *testing.T) {
	app, tree, stateDir := scaffoldFixture(t)
	res, err := syncSkills(app, stateDir, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 targets, got %+v", res)
	}
	for _, r := range res {
		if r.Action != skillCreated {
			t.Fatalf("want created, got %+v", r)
		}
	}
	op := filepath.Join(tree, ".claude", "skills", "lever-operator", "SKILL.md")
	ag := filepath.Join(tree, "workers", "scratch", ".claude", "skills", "lever-agent", "SKILL.md")
	for _, p := range []string{op, ag} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("missing scaffold %s: %v", p, err)
		}
		if !strings.Contains(string(b), "lever-version: "+Version) {
			t.Fatalf("%s missing version stamp", p)
		}
	}
	st := readState(t, stateDir)
	if st[".claude/skills/lever-operator/SKILL.md"] != skills.Hash(skills.Operator(Version)) {
		t.Fatalf("state hash mismatch: %+v", st)
	}
}

func TestSyncSkillsIdempotentThenOwnerEditGuard(t *testing.T) {
	app, tree, stateDir := scaffoldFixture(t)
	if _, err := syncSkills(app, stateDir, false, false); err != nil {
		t.Fatal(err)
	}
	res, _ := syncSkills(app, stateDir, false, false)
	for _, r := range res {
		if r.Action != skillUnchanged {
			t.Fatalf("rerun must be unchanged, got %+v", r)
		}
	}
	// Owner edit: skipped without force, forced with force.
	op := filepath.Join(tree, ".claude", "skills", "lever-operator", "SKILL.md")
	if err := os.WriteFile(op, []byte("my own notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ = syncSkills(app, stateDir, false, false)
	if res[0].Action != skillSkipped {
		t.Fatalf("want skipped-modified, got %+v", res[0])
	}
	if b, _ := os.ReadFile(op); string(b) != "my own notes" {
		t.Fatal("skip must not touch the owner-edited file")
	}
	res, _ = syncSkills(app, stateDir, true, false)
	if res[0].Action != skillForced {
		t.Fatalf("want forced, got %+v", res[0])
	}
}

func TestSyncSkillsCheckModeReportsWithoutWriting(t *testing.T) {
	app, tree, stateDir := scaffoldFixture(t)
	res, err := syncSkills(app, stateDir, false, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Action != skillMissing {
			t.Fatalf("want missing, got %+v", r)
		}
	}
	if _, err := os.Stat(filepath.Join(tree, ".claude")); !os.IsNotExist(err) {
		t.Fatal("check mode must not write")
	}
}

func TestEnsureClaudeMDBlock(t *testing.T) {
	_, tree, _ := scaffoldFixture(t)
	// No CLAUDE.md → created.
	act, err := ensureClaudeMDBlock(tree, false)
	if err != nil || act != skillCreated {
		t.Fatalf("create: act=%v err=%v", act, err)
	}
	// Idempotent.
	if act, _ = ensureClaudeMDBlock(tree, false); act != skillUnchanged {
		t.Fatalf("rerun: %v", act)
	}
	// Owner text preserved; stale inner content replaced.
	p := filepath.Join(tree, "CLAUDE.md")
	b, _ := os.ReadFile(p)
	mutated := "# Mine\n\n" + strings.Replace(string(b), "Consult it", "OLD TEXT", 1) + "\ntrailing owner text\n"
	if err := os.WriteFile(p, []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
	if act, _ = ensureClaudeMDBlock(tree, false); act != skillRefreshed {
		t.Fatalf("refresh: %v", act)
	}
	after, _ := os.ReadFile(p)
	s := string(after)
	if !strings.Contains(s, "# Mine") || !strings.Contains(s, "trailing owner text") {
		t.Fatal("owner text clobbered")
	}
	if strings.Contains(s, "OLD TEXT") || !strings.Contains(s, "Consult it") {
		t.Fatal("block not refreshed")
	}
	// Check mode on a tree with no CLAUDE.md.
	tree2 := t.TempDir()
	if act, _ = ensureClaudeMDBlock(tree2, true); act != skillMissing {
		t.Fatalf("check-missing: %v", act)
	}
}
