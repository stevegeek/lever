package config

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/stevegeek/lever/internal/wire"
)

// ClaudeSettings is the allowlisted part of an agent's Claude Code settings
// that lever delivers (`claude_settings:` under manager or a worker). Each
// key maps to one Claude Code setting; a key not in claudeSettingKeys is a
// load error, so a typo never passes as an unset value. To add a key: add
// the field, its yaml name to claudeSettingKeys, its check to validate, and
// its delivery to wire.Claude and lever-agent boot.
type ClaudeSettings struct {
	// AutoCompactWindow is Claude Code's CLAUDE_CODE_AUTO_COMPACT_WINDOW
	// (the autoCompactWindow setting): the context size, in tokens, at which
	// the session compacts. 0 = unset (Claude Code's own default).
	AutoCompactWindow int `yaml:"auto_compact_window"`
}

// claudeSettingKeys are the keys ClaudeSettings accepts.
var claudeSettingKeys = []string{"auto_compact_window"}

// UnmarshalYAML refuses any key outside claudeSettingKeys (the rest of the
// config decodes leniently), then decodes the known ones.
func (c *ClaudeSettings) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: claude_settings must be a mapping", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if !slices.Contains(claudeSettingKeys, k.Value) {
			return fmt.Errorf("line %d: claude_settings: unknown key %q (lever delivers only: %s)", k.Line, k.Value, strings.Join(claudeSettingKeys, ", "))
		}
	}
	type plain ClaudeSettings
	return n.Decode((*plain)(c))
}

// claudeFor builds the wire form of an agent's Claude config, nil when it
// configures nothing (so the envelope carries no claude block at all).
func claudeFor(s ClaudeSettings, note string) *wire.Claude {
	c := &wire.Claude{AutoCompactWindow: s.AutoCompactWindow, AfterCompactNote: note}
	if c.IsZero() {
		return nil
	}
	return c
}

// ManagerClaude is the manager's Claude Code config for its bootstrap
// envelope; nil when none is set.
func (a *App) ManagerClaude() *wire.Claude {
	return claudeFor(a.Manager.ClaudeSettings, a.Manager.AfterCompactNote)
}

// Claude is worker g's own Claude Code config; nil when none is set.
// Deliberately not inherited from the manager: the window and the note
// describe one agent's work.
func (g Worker) Claude() *wire.Claude {
	return claudeFor(g.ClaudeSettings, g.AfterCompactNote)
}

// validateClaude checks one agent's Claude config; who names the agent in
// the error ("manager", `worker "x"`).
func validateClaude(who string, s ClaudeSettings, note string) error {
	if err := claudeFor(s, note).Validate(); err != nil {
		return fmt.Errorf("config: %s: %w", who, err)
	}
	return nil
}
