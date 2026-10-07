package agent

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/wire"
)

// ManagedSettingsPath, AutoCompactWindowEnv: see wire.
const (
	ManagedSettingsPath  = wire.ManagedSettingsPath
	AutoCompactWindowEnv = wire.AutoCompactWindowEnv
	afterCompactCommand  = wire.AfterCompactHookPrefix
)

// AfterCompactMarker starts the context lever's after-compaction hook gives
// the agent. It arrives as SessionStart hook context, which only the
// container's own Claude Code config can produce, never inside a message;
// the lever-agent skill says so, so a message that types the marker is
// read as the sender's words.
const AfterCompactMarker = "[lever: operator note after compaction]"

// WriteManagedSettings merges c into the managed-settings file at path:
// AutoCompactWindow as env.CLAUDE_CODE_AUTO_COMPACT_WINDOW, and
// AfterCompactNote as a SessionStart hook with matcher "compact" (Claude
// Code's source for the session start that follows a compaction). Keys and
// hooks lever does not own are kept; lever's are replaced, or removed when c
// no longer sets them. nil c with no file is a no-op, so an agent with no
// claude config is never touched. An empty path is a no-op (enrol-only boot).
//
// The file is replaced, not rewritten in place: removed, then created 0644,
// so it is root's whoever owned the old one. Like WriteSettingsEnv, the work
// goes through an os.Root two levels up (/etc), refusing a symbolic link at
// /etc/claude-code or at the file.
func WriteManagedSettings(path string, c *wire.Claude) error {
	if path == "" {
		return nil
	}
	root, rel := splitAbove(path, 2)
	r, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("managed settings %s: open %s: %w", path, root, err)
	}
	defer r.Close()
	if err := checkedFile(r, rel); err != nil {
		return err
	}
	settings := map[string]any{}
	existed := false
	if b, err := r.ReadFile(rel); err == nil {
		existed = true
		if err := json.Unmarshal(b, &settings); err != nil {
			return fmt.Errorf("managed settings %s: parse existing: %w", path, err)
		}
		if settings == nil { // the file held `null`
			settings = map[string]any{}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("managed settings %s: read: %w", path, err)
	}
	if !existed && c.IsZero() {
		return nil
	}
	mergeManagedSettings(settings, c)
	if len(settings) == 0 {
		// Nothing but lever's keys was there: leave no file behind.
		if err := r.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("managed settings %s: remove: %w", path, err)
		}
		return nil
	}
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if d := filepath.Dir(rel); d != "." {
		if err := r.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("managed settings dir %s: %w", filepath.Dir(path), err)
		}
	}
	if err := r.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("managed settings %s: remove old: %w", path, err)
	}
	return r.WriteFile(rel, b, 0o644)
}

// mergeManagedSettings applies c to settings in place (see
// WriteManagedSettings). A key of the wrong JSON type where lever writes
// (env not an object, SessionStart not an array) is replaced: it is not a
// setting Claude Code could read anyway.
func mergeManagedSettings(settings map[string]any, c *wire.Claude) {
	var window int
	var note string
	if c != nil {
		window, note = c.AutoCompactWindow, c.AfterCompactNote
	}

	env, _ := settings["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	delete(env, AutoCompactWindowEnv)
	if window > 0 {
		env[AutoCompactWindowEnv] = strconv.Itoa(window)
	}
	setOrDelete(settings, "env", env, len(env) > 0)

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	groups, _ := hooks["SessionStart"].([]any)
	kept := make([]any, 0, len(groups)+1)
	for _, g := range groups {
		if !isLeverAfterCompactGroup(g) {
			kept = append(kept, g)
		}
	}
	if note != "" {
		kept = append(kept, map[string]any{
			"matcher": "compact",
			"hooks": []any{map[string]any{
				"type":    "command",
				"command": AfterCompactHookCommand(note),
			}},
		})
	}
	setOrDelete(hooks, "SessionStart", kept, len(kept) > 0)
	setOrDelete(settings, "hooks", hooks, len(hooks) > 0)
}

func setOrDelete(m map[string]any, key string, v any, keep bool) {
	if keep {
		m[key] = v
	} else {
		delete(m, key)
	}
}

// isLeverAfterCompactGroup reports whether a SessionStart hook group holds
// lever's after-compaction hook.
func isLeverAfterCompactGroup(g any) bool {
	gm, _ := g.(map[string]any)
	hs, _ := gm["hooks"].([]any)
	for _, h := range hs {
		hm, _ := h.(map[string]any)
		if cmd, _ := hm["command"].(string); strings.HasPrefix(cmd, afterCompactCommand) {
			return true
		}
	}
	return false
}

// AfterCompactHookCommand is the hook command that prints note.
func AfterCompactHookCommand(note string) string {
	return afterCompactCommand + base64.StdEncoding.EncodeToString([]byte(note))
}

// AfterCompactOutput is what the after-compaction hook prints: Claude Code's
// SessionStart hook JSON, whose additionalContext the session receives. The
// note is checked again here (wire.ValidateAfterCompactNote): the command
// line is in a file the agent may have rewritten.
func AfterCompactOutput(noteB64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(noteB64)
	if err != nil {
		return nil, fmt.Errorf("after-compact: note is not base64: %w", err)
	}
	note := string(raw)
	if err := wire.ValidateAfterCompactNote(note); err != nil {
		return nil, fmt.Errorf("after-compact: %w", err)
	}
	if note == "" {
		return nil, errors.New("after-compact: empty note")
	}
	type specific struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	}
	return json.Marshal(struct {
		HookSpecificOutput specific `json:"hookSpecificOutput"`
	}{specific{"SessionStart", AfterCompactMarker + " " + note}})
}
