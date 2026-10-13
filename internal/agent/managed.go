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

// ManagedSettingsPath, AutoCompactWindowEnv, DisableAgentViewEnv,
// DisableAgentViewSetting: see wire.
const (
	ManagedSettingsPath     = wire.ManagedSettingsPath
	AutoCompactWindowEnv    = wire.AutoCompactWindowEnv
	DisableAgentViewEnv     = wire.DisableAgentViewEnv
	DisableAgentViewSetting = wire.DisableAgentViewSetting
	afterCompactCommand     = wire.AfterCompactHookPrefix
)

// AfterCompactMarker starts the context lever's after-compaction hook gives
// the agent, as SessionStart hook context, never inside a message. It
// deliberately does not start with "[lever: operator note", the skills'
// marker for a trusted operator note, so a contact who types it in a message
// matches nothing trusted.
const AfterCompactMarker = "[after compaction: note from this instance's lever.yaml]"

// WriteManagedSettings merges c into the managed-settings file at path. It
// always disables agent view and adds the claude-ready hook, so the file is
// written for every agent, nil c included. Keys and hooks lever does not own
// are kept; lever's are replaced, or removed when c no longer sets them. An
// empty path is a no-op.
//
// The file is removed, then created 0644 with O_EXCL, so a planted symbolic
// link is refused. The work goes through an os.Root two levels up, refusing a
// symbolic link at /etc/claude-code or at the file.
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
	if b, err := r.ReadFile(rel); err == nil {
		if err := json.Unmarshal(b, &settings); err != nil {
			return fmt.Errorf("managed settings %s: parse existing: %w", path, err)
		}
		if settings == nil { // the file held `null`
			settings = map[string]any{}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("managed settings %s: read: %w", path, err)
	}
	mergeManagedSettings(settings, c)
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
	// O_EXCL: a symbolic link the agent plants between the Remove and this
	// create is refused, never followed.
	f, err := r.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("managed settings %s: create: %w", path, err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("managed settings %s: write: %w", path, err)
	}
	return f.Close()
}

// mergeManagedSettings replaces a key of the wrong JSON type where lever
// writes: Claude Code could not read it anyway.
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
	// See wire.DisableAgentViewEnv.
	env[DisableAgentViewEnv] = "1"
	settings["env"] = env
	settings[DisableAgentViewSetting] = true

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	groups, _ := hooks["SessionStart"].([]any)
	kept := make([]any, 0, len(groups)+1)
	for _, g := range groups {
		if !isLeverAfterCompactGroup(g) && !isLeverReadyGroup(g) {
			kept = append(kept, g)
		}
	}
	// No matcher: every session start, including --continue, /clear and a
	// compaction.
	kept = append(kept, map[string]any{
		"hooks": []any{map[string]any{
			"type":    "command",
			"command": ReadyHookCommand,
		}},
	})
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

func isLeverReadyGroup(g any) bool {
	gm, _ := g.(map[string]any)
	hs, _ := gm["hooks"].([]any)
	for _, h := range hs {
		hm, _ := h.(map[string]any)
		if cmd, _ := hm["command"].(string); cmd == ReadyHookCommand {
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
// SessionStart hook JSON. The note is validated again here: the command line
// is in a file the agent may have rewritten.
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
