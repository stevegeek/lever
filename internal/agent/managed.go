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
// the agent. It arrives as SessionStart hook context, never inside a
// message. The skills do not name it, and it deliberately does not start
// with "[lever: operator note": that prefix is the skills' older marker for
// a trusted operator note, and a contact who types this marker in a message
// must not match it. A message that carries it falls under the skills'
// general rule (a marker in a message proves nothing).
const AfterCompactMarker = "[after compaction: note from this instance's lever.yaml]"

// WriteManagedSettings merges c into the managed-settings file at path:
// AutoCompactWindow as env.CLAUDE_CODE_AUTO_COMPACT_WINDOW, and
// AfterCompactNote as a SessionStart hook with matcher "compact" (Claude
// Code's source for the session start that follows a compaction). It also
// always turns off background agents and agent view
// (env.CLAUDE_CODE_DISABLE_AGENT_VIEW=1 and disableAgentView: true; see
// wire.DisableAgentViewEnv) and always adds lever's claude-ready
// SessionStart hook (wire.ReadyHookCommand), whatever c holds, so the file
// is written for every agent, nil c included. Keys and hooks lever does not own are kept;
// lever's are replaced, or removed when c no longer sets them. An empty path
// is a no-op (enrol-only boot).
//
// The file is replaced, not rewritten in place: removed, then created 0644
// with O_EXCL, so a planted symbolic link is refused. Like WriteSettingsEnv, the work
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
	// create (it owns /etc/claude-code) is refused, never followed.
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
	// Always, for every agent: a background session breaks the next
	// claude --continue after a container restart (wire.DisableAgentViewEnv).
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
	// Always, for every agent: the claude-ready hook, with no matcher (every
	// session start: a new session, a --continue, a /clear, a compaction).
	// The manager gate waits for its marker after a start (lever#188).
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

// isLeverReadyGroup reports whether a SessionStart hook group holds lever's
// claude-ready hook.
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
