package config

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/stevegeek/lever/internal/wire"
)

// ClaudeSettings is the allowlisted part of an agent's Claude Code settings
// that lever delivers. An unknown key is a load error, so a typo never
// passes as an unset value. A new key also needs its check in validate and
// its delivery in wire.Claude and lever-agent boot.
type ClaudeSettings struct {
	// AutoCompactWindow is Claude Code's CLAUDE_CODE_AUTO_COMPACT_WINDOW, in
	// tokens.
	AutoCompactWindow int `yaml:"auto_compact_window"`
}

var claudeSettingKeys = []string{"auto_compact_window"}

// UnmarshalYAML is strict, unlike the rest of the config.
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

// Claude is worker g's own Claude Code config, not inherited from the
// manager: the window and the note describe one agent's work.
func (g Worker) Claude() *wire.Claude {
	return claudeFor(g.ClaudeSettings, g.AfterCompactNote)
}

func validateClaude(who string, s ClaudeSettings, note string) error {
	if err := claudeFor(s, note).Validate(); err != nil {
		return fmt.Errorf("config: %s: %w", who, err)
	}
	return nil
}
