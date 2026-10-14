package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/sessionrec"
	"github.com/stevegeek/lever/internal/skills"
	"github.com/stevegeek/lever/internal/state"
)

// The scaffold engine behind `lever init` and doctor's skills check. It writes
// the embedded skills into the instance tree and hash-guards owner edits with
// hashes in state.State.Skills, with owner-blessed customizations in
// state.State.SkillsAdopted (`init --adopt`). Every tree read and write goes
// through fsutil.ReadInTree/WriteInTree: the tree is agent-writable, so a
// scaffold path replaced by a symlink out of the tree is refused, not followed.

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
	// AdoptedVersion is the `lever-version:` stamp of an adopted file (empty = no
	// stamp). An adoption pins the file to the baseline it was adopted at, so
	// doctor compares this against the current version.
	AdoptedVersion string
}

type skillTarget struct {
	relPath string // relative to tree, forward slashes
	content []byte // rendered
}

// skillTargets is every agent's lever skill, at sessionrec.SkillRel (the
// session record hashes the same file).
func skillTargets(app *config.App) []skillTarget {
	rel, _ := sessionrec.SkillRel(app, app.Name)
	ts := []skillTarget{{relPath: rel, content: skills.OperatorWith(cli.Version, brokerctl.ChatConfigured(app), app.AgentMessagesOn(), skillFiles(app), app.AnyRecyclableWorker())}}
	for _, g := range app.Workers {
		rel, _ := sessionrec.SkillRel(app, g.Name)
		ts = append(ts, skillTarget{relPath: rel, content: skills.AgentWith(cli.Version, brokerctl.ChatConfigured(app), app.AgentMessagesOn(), skillFiles(app))})
	}
	return ts
}

// Both hash files live host-side, outside the mounted tree, so an agent cannot
// re-bless its own tampering; adoption is kept separate from scaffold tracking.
const (
	// claudeMDAdoptKey is the adoption key for the tree-root CLAUDE.md (a
	// whole-file hash, not just the lever block).
	claudeMDAdoptKey = "CLAUDE.md"
)

func loadHashState(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return m, nil
}

// saveHashState writes 0644 (hashes are not secrets), atomically, so a crash
// never leaves a torn record that misreports every skill as edited.
func saveHashState(path string, m map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(b, '\n'), 0o644)
}

func loadSkillState(st state.State) (map[string]string, error) {
	return loadHashState(st.Skills())
}

func saveSkillState(st state.State, m map[string]string) error {
	return saveHashState(st.Skills(), m)
}

func loadAdoptedState(st state.State) (map[string]string, error) {
	return loadHashState(st.SkillsAdopted())
}

func saveAdoptedState(st state.State, m map[string]string) error {
	return saveHashState(st.SkillsAdopted(), m)
}

func syncSkills(app *config.App, stateDir state.State, force, check bool) ([]skillSyncResult, error) {
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
		want := skills.Hash(tgt.content)
		recorded, hasRecord := st[tgt.relPath]
		adoptedHash, hasAdopted := adopted[tgt.relPath]
		onDisk, readErr := fsutil.ReadInTree(app.Tree, tgt.relPath)
		exists := readErr == nil
		if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", tgt.relPath, readErr)
		}
		var onDiskHash string
		if exists {
			onDiskHash = skills.Hash(onDisk)
		}
		act, write := decideSkill(exists, onDiskHash == want,
			hasRecord && onDiskHash == recorded,
			hasAdopted && onDiskHash == adoptedHash, force, check)
		if write {
			if err := fsutil.WriteInTree(app.Tree, tgt.relPath, tgt.content, 0o644); err != nil {
				return nil, fmt.Errorf("write %s: %w", tgt.relPath, err)
			}
			// The content is the framework scaffold again: an adoption record would bless
			// content that no longer exists.
			if hasAdopted {
				delete(adopted, tgt.relPath)
				adoptedDirty = true
			}
		}
		// Record the hash for every non-skip outcome in write mode, so matching
		// content gets tracked too. Adopted files stay off skills.json: their content
		// is not lever's.
		if !check && act != skillSkipped && act != skillAdopted {
			if st[tgt.relPath] != want {
				st[tgt.relPath] = want
				dirty = true
			}
		}
		res := skillSyncResult{RelPath: tgt.relPath, Action: act}
		if act == skillAdopted {
			res.AdoptedVersion = skills.LeverVersion(onDisk)
		}
		results = append(results, res)
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

// decideSkill: current content always wins; an adopted customization is kept
// (only --force reclaims it); an unmodified scaffold refreshes silently;
// anything else, an untracked file included, is an owner edit.
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

// writeClaudeMDWithBlock writes CLAUDE.md with the current marker block,
// replaced in place or appended. Written in place, not by rename, so an
// in-tree symlink keeps working and its mode; a link out of the tree is
// refused (fsutil.ErrEscapesTree).
func writeClaudeMDWithBlock(tree, s, block string) error {
	begin := strings.Index(s, skillMarkerBegin)
	end := strings.Index(s, skillMarkerEnd)
	if begin == -1 || end == -1 || end < begin {
		return writeClaudeMD(tree, s+"\n"+block+"\n")
	}
	return writeClaudeMD(tree, s[:begin]+block+s[end+len(skillMarkerEnd):])
}

func writeClaudeMD(tree, content string) error {
	if err := fsutil.WriteInTree(tree, claudeMDAdoptKey, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", claudeMDAdoptKey, err)
	}
	return nil
}

// readClaudeMD reads the agent-writable CLAUDE.md through the tree
// confinement. Absent is fs.ErrNotExist.
func readClaudeMD(tree string) ([]byte, error) {
	b, err := fsutil.ReadInTree(tree, claudeMDAdoptKey)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", claudeMDAdoptKey, err)
	}
	return b, err
}

func claudeMDBlockCurrent(s string) bool {
	begin := strings.Index(s, skillMarkerBegin)
	end := strings.Index(s, skillMarkerEnd)
	return begin != -1 && end != -1 && end >= begin &&
		s[begin:end+len(skillMarkerEnd)] == claudeMDBlock()
}

func ensureClaudeMDBlock(tree string, stateDir state.State, force, check bool) (skillAction, error) {
	adopted, err := loadAdoptedState(stateDir)
	if err != nil {
		return "", err
	}
	block := claudeMDBlock()
	b, err := readClaudeMD(tree)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if check {
			return skillMissing, nil
		}
		// A record for a deleted file would bless whatever is written next.
		if _, ok := adopted[claudeMDAdoptKey]; ok {
			delete(adopted, claudeMDAdoptKey)
			if err := saveAdoptedState(stateDir, adopted); err != nil {
				return "", err
			}
		}
		return skillCreated, writeClaudeMD(tree, block+"\n")
	case err != nil:
		return "", err
	}
	s := string(b)
	if rec, ok := adopted[claudeMDAdoptKey]; ok {
		if check || !force {
			if skills.Hash(b) == rec {
				return skillAdopted, nil
			}
			// Drift after adoption is owner territory or tampering: never write without
			// force; doctor tells them apart.
			return skillSkipped, nil
		}
		// --force reclaims adopted territory: restore the block and drop the record.
		delete(adopted, claudeMDAdoptKey)
		if err := saveAdoptedState(stateDir, adopted); err != nil {
			return "", err
		}
		return skillForced, writeClaudeMDWithBlock(tree, s, block)
	}
	begin := strings.Index(s, skillMarkerBegin)
	end := strings.Index(s, skillMarkerEnd)
	if begin == -1 || end == -1 || end < begin {
		if check {
			return skillMissing, nil
		}
		return skillCreated, writeClaudeMDWithBlock(tree, s, block)
	}
	current := s[begin : end+len(skillMarkerEnd)]
	if current == block {
		return skillUnchanged, nil
	}
	if check {
		return skillStale, nil
	}
	return skillRefreshed, writeClaudeMDWithBlock(tree, s, block)
}

// skillsUpToDate counts an adopted customization as OK: a deliberate owner
// decision.
func skillsUpToDate(results []skillSyncResult, blockAction skillAction) bool {
	for _, r := range results {
		if r.Action != skillUnchanged && r.Action != skillAdopted {
			return false
		}
	}
	return blockAction == skillUnchanged || blockAction == skillAdopted
}

// adoptSkills records the on-disk hash of every owner-customized scaffold (and
// the whole CLAUDE.md) as an accepted baseline. Recording rather than muting
// keeps doctor's tamper detection: the files are agent-writable, the baseline
// is not. Framework-current content needs no baseline, and an unmodified stale
// scaffold is refreshed, not adopted (that would pin old content).
func adoptSkills(app *config.App, stateDir state.State) ([]skillSyncResult, error) {
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
		onDisk, readErr := fsutil.ReadInTree(app.Tree, tgt.relPath)
		switch {
		case errors.Is(readErr, fs.ErrNotExist):
			results = append(results, skillSyncResult{RelPath: tgt.relPath, Action: skillMissing})
			continue
		case readErr != nil:
			return nil, fmt.Errorf("read %s: %w", tgt.relPath, readErr)
		}
		onDiskHash := skills.Hash(onDisk)
		if onDiskHash == skills.Hash(tgt.content) {
			// Framework-current beats adopted; a stale record would pin an old baseline.
			unrecord(tgt.relPath)
			results = append(results, skillSyncResult{RelPath: tgt.relPath, Action: skillUnchanged})
			continue
		}
		if rec, ok := st[tgt.relPath]; ok && rec == onDiskHash {
			results = append(results, skillSyncResult{RelPath: tgt.relPath, Action: skillStale})
			continue
		}
		record(tgt.relPath, onDiskHash)
		results = append(results, skillSyncResult{RelPath: tgt.relPath, Action: skillAdopted})
	}
	// CLAUDE.md is adopted whole, block or not (omitting the block can be the
	// point). A file with the current block needs no baseline, and a record would
	// freeze block refreshes.
	b, err := readClaudeMD(app.Tree)
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

func skillFiles(app *config.App) skills.Files {
	return skills.Files{On: app.FilesOn(), NoUploads: app.FilesOn() && !app.FilesUploadsOn(), NoShares: app.FilesOn() && !app.FilesSharesOn()}
}
