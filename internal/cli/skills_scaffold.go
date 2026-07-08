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
// hashes recorded in .lever-state/skills.json. Pure file operations — no jail
// interaction.

type skillAction string

const (
	skillCreated   skillAction = "created"
	skillRefreshed skillAction = "refreshed"
	skillUnchanged skillAction = "unchanged"
	skillSkipped   skillAction = "skipped-modified"
	skillForced    skillAction = "forced"
	skillMissing   skillAction = "missing"
	skillStale     skillAction = "stale"
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

const skillStateFile = "skills.json"

func loadSkillState(stateDir string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, skillStateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", skillStateFile, err)
	}
	return m, nil
}

func saveSkillState(stateDir string, st map[string]string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, skillStateFile), append(b, '\n'), 0o644)
}

func syncSkills(app *config.App, stateDir string, force, check bool) ([]skillSyncResult, error) {
	st, err := loadSkillState(stateDir)
	if err != nil {
		return nil, err
	}
	var results []skillSyncResult
	dirty := false
	for _, tgt := range skillTargets(app) {
		abs := filepath.Join(app.Tree, filepath.FromSlash(tgt.relPath))
		want := skills.Hash(tgt.content)
		recorded, hasRecord := st[tgt.relPath]
		onDisk, readErr := os.ReadFile(abs)
		exists := readErr == nil
		if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", abs, readErr)
		}
		act, write := decideSkill(exists, exists && skills.Hash(onDisk) == want,
			exists && hasRecord && skills.Hash(onDisk) == recorded, force, check)
		if write {
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(abs, tgt.content, 0o644); err != nil {
				return nil, err
			}
		}
		// Record/refresh the hash for every non-skip outcome in write mode, so
		// pre-init adopters whose content happens to match get tracked too.
		if !check && act != skillSkipped {
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
	return results, nil
}

// decideSkill maps the observed target state to (action, writeNeeded). See
// the behavior table in the plan/spec: current content always wins; an
// unmodified scaffold refreshes silently; anything else — including an
// untracked pre-existing file — is treated as an owner edit.
func decideSkill(exists, isCurrent, matchesRecord, force, check bool) (skillAction, bool) {
	switch {
	case !exists:
		if check {
			return skillMissing, false
		}
		return skillCreated, true
	case isCurrent:
		return skillUnchanged, false
	case matchesRecord: // unmodified scaffold, content moved on
		if check {
			return skillStale, false
		}
		return skillRefreshed, true
	default: // owner-edited (or untracked pre-existing file)
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

func ensureClaudeMDBlock(tree string, check bool) (skillAction, error) {
	path := filepath.Join(tree, "CLAUDE.md")
	block := claudeMDBlock()
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if check {
			return skillMissing, nil
		}
		return skillCreated, os.WriteFile(path, []byte(block+"\n"), 0o644)
	case err != nil:
		return "", err
	}
	s := string(b)
	begin := strings.Index(s, skillMarkerBegin)
	end := strings.Index(s, skillMarkerEnd)
	if begin == -1 || end == -1 || end < begin {
		if check {
			return skillMissing, nil
		}
		return skillCreated, os.WriteFile(path, []byte(s+"\n"+block+"\n"), 0o644)
	}
	current := s[begin : end+len(skillMarkerEnd)]
	if current == block {
		return skillUnchanged, nil
	}
	if check {
		return skillStale, nil
	}
	return skillRefreshed, os.WriteFile(path, []byte(s[:begin]+block+s[end+len(skillMarkerEnd):]), 0o644)
}

// skillsUpToDate is doctor's predicate over a check-mode run.
func skillsUpToDate(results []skillSyncResult, blockAction skillAction) bool {
	for _, r := range results {
		if r.Action != skillUnchanged {
			return false
		}
	}
	return blockAction == skillUnchanged
}
