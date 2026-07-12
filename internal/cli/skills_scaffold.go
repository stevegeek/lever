package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/skills"
)

// The scaffold engine behind `lever init` and doctor's skills check: writes
// the embedded skills into the instance tree, hash-guarding owner edits via
// hashes recorded in .lever-state/skills.json, with owner-blessed
// customizations tracked in .lever-state/skills-adopted.json (`init --adopt`).
// Pure file operations — no jail interaction.

type skillAction string

const (
	skillCreated   skillAction = "created"
	skillRefreshed skillAction = "refreshed"
	skillUnchanged skillAction = "unchanged"
	skillSkipped   skillAction = "skipped-modified"
	skillForced    skillAction = "forced"
	skillMissing   skillAction = "missing"
	skillStale     skillAction = "stale"
	skillAdopted   skillAction = "custom-adopted"
)

type skillSyncResult struct {
	RelPath string
	Action  skillAction
}

type skillTarget struct {
	relPath string // relative to tree, forward slashes
	content []byte // rendered
}

func skillTargets(app *config.App) []skillTarget {
	ts := []skillTarget{{relPath: ".claude/skills/lever-operator/SKILL.md", content: skills.Operator(Version)}}
	for _, g := range app.Workers {
		rel := filepath.ToSlash(filepath.Join(g.Dir, ".claude", "skills", "lever-agent", "SKILL.md"))
		ts = append(ts, skillTarget{relPath: rel, content: skills.Agent(Version)})
	}
	return ts
}

const (
	skillStateFile = "skills.json"
	// skillAdoptedFile records owner-blessed customizations (path → hash of
	// the adopted on-disk content). Separate from skills.json so adoption
	// never disturbs the scaffold-tracking schema. Lives host-side, outside
	// the mounted tree, so an agent cannot re-bless its own tampering.
	skillAdoptedFile = "skills-adopted.json"
	// claudeMDAdoptKey is the adoption-map key for the tree-root CLAUDE.md
	// (whole-file hash — adoption covers the file, not just the lever block).
	claudeMDAdoptKey = "CLAUDE.md"
)

func loadHashState(stateDir, file string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, file))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	return m, nil
}

func saveHashState(stateDir, file string, st map[string]string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, file), append(b, '\n'), 0o644)
}

func loadSkillState(stateDir string) (map[string]string, error) {
	return loadHashState(stateDir, skillStateFile)
}

func saveSkillState(stateDir string, st map[string]string) error {
	return saveHashState(stateDir, skillStateFile, st)
}

func loadAdoptedState(stateDir string) (map[string]string, error) {
	return loadHashState(stateDir, skillAdoptedFile)
}

func saveAdoptedState(stateDir string, st map[string]string) error {
	return saveHashState(stateDir, skillAdoptedFile, st)
}

func syncSkills(app *config.App, stateDir string, force, check bool) ([]skillSyncResult, error) {
	st, err := loadSkillState(stateDir)
	if err != nil {
		return nil, err
	}
	adopted, err := loadAdoptedState(stateDir)
	if err != nil {
		return nil, err
	}
	var results []skillSyncResult
	dirty, adoptedDirty := false, false
	for _, tgt := range skillTargets(app) {
		abs := filepath.Join(app.Tree, filepath.FromSlash(tgt.relPath))
		want := skills.Hash(tgt.content)
		recorded, hasRecord := st[tgt.relPath]
		adoptedHash, hasAdopted := adopted[tgt.relPath]
		onDisk, readErr := os.ReadFile(abs)
		exists := readErr == nil
		if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", abs, readErr)
		}
		var onDiskHash string
		if exists {
			onDiskHash = skills.Hash(onDisk)
		}
		act, write := decideSkill(exists, onDiskHash == want,
			hasRecord && onDiskHash == recorded,
			hasAdopted && onDiskHash == adoptedHash, force, check)
		if write {
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(abs, tgt.content, 0o644); err != nil {
				return nil, err
			}
			// Content is the framework scaffold again — any adoption record
			// would bless content that no longer exists.
			if hasAdopted {
				delete(adopted, tgt.relPath)
				adoptedDirty = true
			}
		}
		// Record/refresh the hash for every non-skip outcome in write mode, so
		// pre-init adopters whose content happens to match get tracked too.
		// Adopted files stay off skills.json — their content isn't ours.
		if !check && act != skillSkipped && act != skillAdopted {
			if st[tgt.relPath] != want {
				st[tgt.relPath] = want
				dirty = true
			}
		}
		results = append(results, skillSyncResult{RelPath: tgt.relPath, Action: act})
	}
	if dirty {
		if err := saveSkillState(stateDir, st); err != nil {
			return nil, err
		}
	}
	if adoptedDirty {
		if err := saveAdoptedState(stateDir, adopted); err != nil {
			return nil, err
		}
	}
	return results, nil
}

// decideSkill maps the observed target state to (action, writeNeeded). See
// the behavior table in the plan/spec: current content always wins; an
// adopted customization is respected (only --force reclaims it); an
// unmodified scaffold refreshes silently; anything else — including an
// untracked pre-existing file — is treated as an owner edit.
func decideSkill(exists, isCurrent, matchesRecord, matchesAdopted, force, check bool) (skillAction, bool) {
	switch {
	case !exists:
		if check {
			return skillMissing, false
		}
		return skillCreated, true
	case isCurrent:
		return skillUnchanged, false
	case matchesAdopted: // owner-blessed customization
		if !check && force {
			return skillForced, true
		}
		return skillAdopted, false
	case matchesRecord: // unmodified scaffold, content moved on
		if check {
			return skillStale, false
		}
		return skillRefreshed, true
	default: // owner-edited (or untracked pre-existing file, or drift after adoption)
		if check || !force {
			return skillSkipped, false
		}
		return skillForced, true
	}
}

const (
	skillMarkerBegin = "<!-- lever:skills:begin -->"
	skillMarkerEnd   = "<!-- lever:skills:end -->"
)

func claudeMDBlock() string {
	return skillMarkerBegin + "\n" +
		"## Lever operator skill\n\n" +
		"Operating inside lever (brokered tools, capabilities, messaging, workers) is\n" +
		"documented in the `lever-operator` skill (`.claude/skills/lever-operator/`).\n" +
		"Consult it before using any brokered MCP tool.\n" +
		skillMarkerEnd
}

// writeClaudeMDWithBlock rewrites CLAUDE.md so it carries the current marker
// block: replaced in place when markers exist, appended otherwise.
func writeClaudeMDWithBlock(path, s, block string) error {
	begin := strings.Index(s, skillMarkerBegin)
	end := strings.Index(s, skillMarkerEnd)
	if begin == -1 || end == -1 || end < begin {
		return os.WriteFile(path, []byte(s+"\n"+block+"\n"), 0o644)
	}
	return os.WriteFile(path, []byte(s[:begin]+block+s[end+len(skillMarkerEnd):]), 0o644)
}

// claudeMDBlockCurrent reports whether s carries the current marker block.
func claudeMDBlockCurrent(s string) bool {
	begin := strings.Index(s, skillMarkerBegin)
	end := strings.Index(s, skillMarkerEnd)
	return begin != -1 && end != -1 && end >= begin &&
		s[begin:end+len(skillMarkerEnd)] == claudeMDBlock()
}

func ensureClaudeMDBlock(tree, stateDir string, force, check bool) (skillAction, error) {
	adopted, err := loadAdoptedState(stateDir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(tree, "CLAUDE.md")
	block := claudeMDBlock()
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if check {
			return skillMissing, nil
		}
		// A record for a deleted file would bless whatever we write next.
		if _, ok := adopted[claudeMDAdoptKey]; ok {
			delete(adopted, claudeMDAdoptKey)
			if err := saveAdoptedState(stateDir, adopted); err != nil {
				return "", err
			}
		}
		return skillCreated, os.WriteFile(path, []byte(block+"\n"), 0o644)
	case err != nil:
		return "", err
	}
	s := string(b)
	if rec, ok := adopted[claudeMDAdoptKey]; ok {
		if check || !force {
			if skills.Hash(b) == rec {
				return skillAdopted, nil
			}
			// Drift after adoption: owner territory (or tampering) — never
			// write without force; doctor surfaces the distinction.
			return skillSkipped, nil
		}
		// --force reclaims adopted territory, same semantics as forced skill
		// files: restore the framework block and drop the now-stale record.
		delete(adopted, claudeMDAdoptKey)
		if err := saveAdoptedState(stateDir, adopted); err != nil {
			return "", err
		}
		return skillForced, writeClaudeMDWithBlock(path, s, block)
	}
	begin := strings.Index(s, skillMarkerBegin)
	end := strings.Index(s, skillMarkerEnd)
	if begin == -1 || end == -1 || end < begin {
		if check {
			return skillMissing, nil
		}
		return skillCreated, writeClaudeMDWithBlock(path, s, block)
	}
	current := s[begin : end+len(skillMarkerEnd)]
	if current == block {
		return skillUnchanged, nil
	}
	if check {
		return skillStale, nil
	}
	return skillRefreshed, writeClaudeMDWithBlock(path, s, block)
}

// skillsUpToDate is doctor's predicate over a check-mode run. An adopted
// customization is a deliberate owner decision, so it counts as OK.
func skillsUpToDate(results []skillSyncResult, blockAction skillAction) bool {
	for _, r := range results {
		if r.Action != skillUnchanged && r.Action != skillAdopted {
			return false
		}
	}
	return blockAction == skillUnchanged || blockAction == skillAdopted
}

// adoptSkills records the on-disk hash of every owner-customized scaffold
// (plus the whole-file CLAUDE.md) as an accepted baseline. Recording rather
// than muting keeps the doctor check useful as tamper detection: the files
// live inside the agent-writable tree, the baseline does not. Only genuine
// customizations qualify — framework-current content needs no baseline, and
// a stale-but-unmodified scaffold is refresh territory, not a customization
// (adopting it would pin old framework content).
func adoptSkills(app *config.App, stateDir string) ([]skillSyncResult, error) {
	st, err := loadSkillState(stateDir)
	if err != nil {
		return nil, err
	}
	adopted, err := loadAdoptedState(stateDir)
	if err != nil {
		return nil, err
	}
	var results []skillSyncResult
	dirty := false
	record := func(rel, hash string) {
		if adopted[rel] != hash {
			adopted[rel] = hash
			dirty = true
		}
	}
	unrecord := func(rel string) {
		if _, ok := adopted[rel]; ok {
			delete(adopted, rel)
			dirty = true
		}
	}
	for _, tgt := range skillTargets(app) {
		abs := filepath.Join(app.Tree, filepath.FromSlash(tgt.relPath))
		onDisk, readErr := os.ReadFile(abs)
		switch {
		case errors.Is(readErr, fs.ErrNotExist):
			results = append(results, skillSyncResult{RelPath: tgt.relPath, Action: skillMissing})
			continue
		case readErr != nil:
			return nil, fmt.Errorf("read %s: %w", abs, readErr)
		}
		onDiskHash := skills.Hash(onDisk)
		if onDiskHash == skills.Hash(tgt.content) {
			// Framework-current beats adopted; a stale record would pin an
			// old baseline over content we own.
			unrecord(tgt.relPath)
			results = append(results, skillSyncResult{RelPath: tgt.relPath, Action: skillUnchanged})
			continue
		}
		if rec, ok := st[tgt.relPath]; ok && rec == onDiskHash {
			// Unmodified old scaffold: `lever init` refreshes it.
			results = append(results, skillSyncResult{RelPath: tgt.relPath, Action: skillStale})
			continue
		}
		record(tgt.relPath, onDiskHash)
		results = append(results, skillSyncResult{RelPath: tgt.relPath, Action: skillAdopted})
	}
	// CLAUDE.md: whole-file adoption, with or without the lever block —
	// adopting a CLAUDE.md that deliberately omits the block is the point.
	// But a file carrying the current block needs no baseline (and a record
	// would freeze future block refreshes).
	b, err := os.ReadFile(filepath.Join(app.Tree, "CLAUDE.md"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		results = append(results, skillSyncResult{RelPath: claudeMDAdoptKey, Action: skillMissing})
	case err != nil:
		return nil, err
	case claudeMDBlockCurrent(string(b)):
		unrecord(claudeMDAdoptKey)
		results = append(results, skillSyncResult{RelPath: claudeMDAdoptKey, Action: skillUnchanged})
	default:
		record(claudeMDAdoptKey, skills.Hash(b))
		results = append(results, skillSyncResult{RelPath: claudeMDAdoptKey, Action: skillAdopted})
	}
	if dirty {
		if err := saveAdoptedState(stateDir, adopted); err != nil {
			return nil, err
		}
	}
	return results, nil
}
