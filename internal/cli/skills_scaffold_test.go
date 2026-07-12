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
	_, tree, stateDir := scaffoldFixture(t)
	// No CLAUDE.md → created.
	act, err := ensureClaudeMDBlock(tree, stateDir, false, false)
	if err != nil || act != skillCreated {
		t.Fatalf("create: act=%v err=%v", act, err)
	}
	// Idempotent.
	if act, _ = ensureClaudeMDBlock(tree, stateDir, false, false); act != skillUnchanged {
		t.Fatalf("rerun: %v", act)
	}
	// Owner text preserved; stale inner content replaced.
	p := filepath.Join(tree, "CLAUDE.md")
	b, _ := os.ReadFile(p)
	mutated := "# Mine\n\n" + strings.Replace(string(b), "Consult it", "OLD TEXT", 1) + "\ntrailing owner text\n"
	if err := os.WriteFile(p, []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
	if act, _ = ensureClaudeMDBlock(tree, stateDir, false, false); act != skillRefreshed {
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
	if act, _ = ensureClaudeMDBlock(tree2, stateDir, false, true); act != skillMissing {
		t.Fatalf("check-missing: %v", act)
	}
}

func readAdopted(t *testing.T, stateDir string) map[string]string {
	t.Helper()
	m, err := loadAdoptedState(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

const opRel = ".claude/skills/lever-operator/SKILL.md"

func TestAdoptRecordsBaselineAndSyncRespectsIt(t *testing.T) {
	app, tree, stateDir := scaffoldFixture(t)
	if _, err := syncSkills(app, stateDir, false, false); err != nil {
		t.Fatal(err)
	}
	op := filepath.Join(tree, filepath.FromSlash(opRel))
	if err := os.WriteFile(op, []byte("my custom operator notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	// CLAUDE.md deliberately WITHOUT the lever block — adopting that is the point.
	cm := filepath.Join(tree, "CLAUDE.md")
	if err := os.WriteFile(cm, []byte("# my own tree docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := adoptSkills(app, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]skillAction{
		opRel: skillAdopted,
		"workers/scratch/.claude/skills/lever-agent/SKILL.md": skillUnchanged, // current, no adoption needed
		"CLAUDE.md": skillAdopted,
	}
	for _, r := range res {
		if want[r.RelPath] != r.Action {
			t.Fatalf("adopt: want %s for %s, got %s", want[r.RelPath], r.RelPath, r.Action)
		}
	}
	ad := readAdopted(t, stateDir)
	if ad[opRel] != skills.Hash([]byte("my custom operator notes")) {
		t.Fatalf("adopted hash not recorded: %+v", ad)
	}
	if _, ok := ad["workers/scratch/.claude/skills/lever-agent/SKILL.md"]; ok {
		t.Fatal("framework-current file must not get an adoption record")
	}

	// Check mode: adopted counts as OK.
	cres, _ := syncSkills(app, stateDir, false, true)
	if cres[0].Action != skillAdopted || cres[1].Action != skillUnchanged {
		t.Fatalf("check after adopt: %+v", cres)
	}
	blockAct, _ := ensureClaudeMDBlock(tree, stateDir, false, true)
	if blockAct != skillAdopted {
		t.Fatalf("block check after adopt: %v", blockAct)
	}
	if !skillsUpToDate(cres, blockAct) {
		t.Fatal("doctor predicate must pass on adopted state")
	}

	// Plain init (write mode): adopted files untouched, no block appended.
	wres, _ := syncSkills(app, stateDir, false, false)
	if wres[0].Action != skillAdopted {
		t.Fatalf("write after adopt: %+v", wres)
	}
	if b, _ := os.ReadFile(op); string(b) != "my custom operator notes" {
		t.Fatal("plain init must not touch an adopted file")
	}
	if blockAct, _ = ensureClaudeMDBlock(tree, stateDir, false, false); blockAct != skillAdopted {
		t.Fatalf("block write after adopt: %v", blockAct)
	}
	if b, _ := os.ReadFile(cm); strings.Contains(string(b), skillMarkerBegin) {
		t.Fatal("plain init must not append the block to an adopted CLAUDE.md")
	}
}

func TestAdoptedThenEditedIsDrift(t *testing.T) {
	app, tree, stateDir := scaffoldFixture(t)
	if _, err := syncSkills(app, stateDir, false, false); err != nil {
		t.Fatal(err)
	}
	op := filepath.Join(tree, filepath.FromSlash(opRel))
	if err := os.WriteFile(op, []byte("blessed"), 0o644); err != nil {
		t.Fatal(err)
	}
	cm := filepath.Join(tree, "CLAUDE.md")
	if err := os.WriteFile(cm, []byte("blessed docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := adoptSkills(app, stateDir); err != nil {
		t.Fatal(err)
	}
	// Post-adoption edits (owner… or agent) → drift, never silently OK.
	if err := os.WriteFile(op, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cm, []byte("tampered docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cres, _ := syncSkills(app, stateDir, false, true)
	if cres[0].Action != skillSkipped {
		t.Fatalf("drifted adoption must check as skipped: %+v", cres)
	}
	if act, _ := ensureClaudeMDBlock(tree, stateDir, false, true); act != skillSkipped {
		t.Fatalf("drifted CLAUDE.md check: %v", act)
	}
	// Write mode must not touch drifted owner territory either.
	if act, _ := ensureClaudeMDBlock(tree, stateDir, false, false); act != skillSkipped {
		t.Fatalf("drifted CLAUDE.md write: %v", act)
	}
	if b, _ := os.ReadFile(cm); string(b) != "tampered docs\n" {
		t.Fatal("write mode must leave a drifted adopted CLAUDE.md alone")
	}
}

func TestForceOverwritesAdoptedAndClearsRecord(t *testing.T) {
	app, tree, stateDir := scaffoldFixture(t)
	if _, err := syncSkills(app, stateDir, false, false); err != nil {
		t.Fatal(err)
	}
	op := filepath.Join(tree, filepath.FromSlash(opRel))
	if err := os.WriteFile(op, []byte("custom"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := adoptSkills(app, stateDir); err != nil {
		t.Fatal(err)
	}
	fres, _ := syncSkills(app, stateDir, true, false)
	if fres[0].Action != skillForced {
		t.Fatalf("force over adopted: %+v", fres)
	}
	if _, ok := readAdopted(t, stateDir)[opRel]; ok {
		t.Fatal("force must clear the stale adoption record")
	}
	cres, _ := syncSkills(app, stateDir, false, true)
	if cres[0].Action != skillUnchanged {
		t.Fatalf("post-force check: %+v", cres)
	}
}

func TestAdoptCurrentFileRemovesStaleRecord(t *testing.T) {
	app, _, stateDir := scaffoldFixture(t)
	if _, err := syncSkills(app, stateDir, false, false); err != nil {
		t.Fatal(err)
	}
	// Plant a stale record for a file that is now framework-current.
	if err := saveAdoptedState(stateDir, map[string]string{opRel: "deadbeef"}); err != nil {
		t.Fatal(err)
	}
	res, err := adoptSkills(app, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != skillUnchanged {
		t.Fatalf("current file must not adopt: %+v", res)
	}
	if _, ok := readAdopted(t, stateDir)[opRel]; ok {
		t.Fatal("current beats adopted — stale record must be removed")
	}
}

func TestAdoptMissingFilesNotAdoptable(t *testing.T) {
	app, _, stateDir := scaffoldFixture(t)
	res, err := adoptSkills(app, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Action != skillMissing {
			t.Fatalf("missing files are not adoptable: %+v", r)
		}
	}
	if len(readAdopted(t, stateDir)) != 0 {
		t.Fatal("nothing must be recorded for missing files")
	}
}

func TestForceRestoresAdoptedClaudeMD(t *testing.T) {
	app, tree, stateDir := scaffoldFixture(t)
	if _, err := syncSkills(app, stateDir, false, false); err != nil {
		t.Fatal(err)
	}
	cm := filepath.Join(tree, "CLAUDE.md")
	if err := os.WriteFile(cm, []byte("# fully custom, no block\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := adoptSkills(app, stateDir); err != nil {
		t.Fatal(err)
	}

	// --force on an intact adopted file: block restored, record cleared.
	act, err := ensureClaudeMDBlock(tree, stateDir, true, false)
	if err != nil || act != skillForced {
		t.Fatalf("force over adopted: act=%v err=%v", act, err)
	}
	b, _ := os.ReadFile(cm)
	if !strings.Contains(string(b), claudeMDBlock()) || !strings.Contains(string(b), "# fully custom") {
		t.Fatalf("force must append the current block, keeping owner text:\n%s", b)
	}
	if _, ok := readAdopted(t, stateDir)[claudeMDAdoptKey]; ok {
		t.Fatal("force must clear the CLAUDE.md adoption record")
	}
	if act, _ = ensureClaudeMDBlock(tree, stateDir, false, true); act != skillUnchanged {
		t.Fatalf("post-force check: %v", act)
	}

	// --force on an adopted-then-tampered file: same reclaim.
	if _, err := adoptSkills(app, stateDir); err != nil { // no-op: block is current now
		t.Fatal(err)
	}
	if err := os.WriteFile(cm, []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Re-adopt the pre-tamper state manually so a record exists, then tamper further.
	if err := saveAdoptedState(stateDir, map[string]string{claudeMDAdoptKey: skills.Hash([]byte("tampered\n"))}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cm, []byte("tampered again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if act, _ = ensureClaudeMDBlock(tree, stateDir, false, true); act != skillSkipped {
		t.Fatalf("tampered adopted file must check as skipped: %v", act)
	}
	if act, err = ensureClaudeMDBlock(tree, stateDir, true, false); err != nil || act != skillForced {
		t.Fatalf("force over tampered adoption: act=%v err=%v", act, err)
	}
	b, _ = os.ReadFile(cm)
	if !strings.Contains(string(b), claudeMDBlock()) {
		t.Fatal("force must restore the block on a tampered adopted file")
	}
	if _, ok := readAdopted(t, stateDir)[claudeMDAdoptKey]; ok {
		t.Fatal("force must clear the tampered adoption record")
	}
	if act, _ = ensureClaudeMDBlock(tree, stateDir, false, true); act != skillUnchanged {
		t.Fatalf("post-force check after tamper: %v", act)
	}
}

func TestAdoptCurrentClaudeMDCreatesNoRecord(t *testing.T) {
	app, tree, stateDir := scaffoldFixture(t)
	if _, err := syncSkills(app, stateDir, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureClaudeMDBlock(tree, stateDir, false, false); err != nil {
		t.Fatal(err)
	}
	// Plant a stale record for the now-current file.
	if err := saveAdoptedState(stateDir, map[string]string{claudeMDAdoptKey: "deadbeef"}); err != nil {
		t.Fatal(err)
	}
	res, err := adoptSkills(app, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	last := res[len(res)-1]
	if last.RelPath != claudeMDAdoptKey || last.Action != skillUnchanged {
		t.Fatalf("current-block CLAUDE.md must not adopt: %+v", last)
	}
	if _, ok := readAdopted(t, stateDir)[claudeMDAdoptKey]; ok {
		t.Fatal("stale CLAUDE.md record must be removed when the block is current")
	}
}

func TestAdoptStaleUnmodifiedScaffoldNotAdopted(t *testing.T) {
	app, tree, stateDir := scaffoldFixture(t)
	if _, err := syncSkills(app, stateDir, false, false); err != nil {
		t.Fatal(err)
	}
	// Simulate a post-upgrade stale scaffold: on-disk == skills.json record != current.
	op := filepath.Join(tree, filepath.FromSlash(opRel))
	old := []byte("old framework scaffold v0")
	if err := os.WriteFile(op, old, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := loadSkillState(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	st[opRel] = skills.Hash(old)
	if err := saveSkillState(stateDir, st); err != nil {
		t.Fatal(err)
	}
	res, err := adoptSkills(app, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != skillStale {
		t.Fatalf("stale-unmodified scaffold must report stale, not adopt: %+v", res)
	}
	if _, ok := readAdopted(t, stateDir)[opRel]; ok {
		t.Fatal("stale-unmodified scaffold must not get an adoption record")
	}
	// `lever init` still refreshes it afterwards.
	sres, _ := syncSkills(app, stateDir, false, false)
	if sres[0].Action != skillRefreshed {
		t.Fatalf("stale scaffold must refresh on plain init: %+v", sres)
	}
}
