package wire

import (
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

// Claude is the per-agent Claude Code configuration the host stages in the
// agent's bootstrap envelope. lever-agent boot writes it into the
// container's managed-settings file at every start, so a value reaches the
// agent at its next start (a resume included), not only at create.
//
// The host validates it at config load, and boot validates it again: the
// manager's envelope sits in the tree it can write.
type Claude struct {
	// AutoCompactWindow becomes CLAUDE_CODE_AUTO_COMPACT_WINDOW: the context
	// size, in tokens, at which Claude Code compacts. 0 means unset.
	AutoCompactWindow int `json:"auto_compact_window,omitempty"`
	// AfterCompactNote is the operator's note Claude Code receives as
	// SessionStart context right after a compaction. "" means unset.
	AfterCompactNote string `json:"after_compact_note,omitempty"`
}

// The bounds of the Claude fields.
const (
	MinAutoCompactWindow     = 100_000
	MaxAutoCompactWindow     = 1_000_000
	MaxAfterCompactNoteBytes = 300
)

// Errors of Claude.Validate.
var (
	ErrAutoCompactWindowRange = fmt.Errorf("auto_compact_window must be between %d and %d tokens", MinAutoCompactWindow, MaxAutoCompactWindow)
	ErrAfterCompactNoteLong   = fmt.Errorf("after_compact_note must be at most %d bytes", MaxAfterCompactNoteBytes)
	ErrAfterCompactNoteText   = errors.New("after_compact_note must be one line of valid UTF-8 with no control or format characters")
)

// IsZero reports whether c configures nothing (nil included).
func (c *Claude) IsZero() bool {
	return c == nil || *c == Claude{}
}

// Validate checks every set field. nil is valid.
func (c *Claude) Validate() error {
	if c == nil {
		return nil
	}
	if c.AutoCompactWindow != 0 && (c.AutoCompactWindow < MinAutoCompactWindow || c.AutoCompactWindow > MaxAutoCompactWindow) {
		return ErrAutoCompactWindowRange
	}
	return ValidateAfterCompactNote(c.AfterCompactNote)
}

// ValidateAfterCompactNote checks a note: at most MaxAfterCompactNoteBytes,
// valid UTF-8, and no control or format character (a newline, a
// bidirectional override), so the note stays one plain line of the agent's
// context. "" is valid (unset).
func ValidateAfterCompactNote(note string) error {
	if len(note) > MaxAfterCompactNoteBytes {
		return ErrAfterCompactNoteLong
	}
	if !utf8.ValidString(note) {
		return ErrAfterCompactNoteText
	}
	for _, r := range note {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return ErrAfterCompactNoteText
		}
	}
	return nil
}

// ManagedSettingsPath is Claude Code's managed-settings file on Linux: the
// highest-precedence settings scope, merged over the user's
// ~/.claude/settings.json (its env keys win, its hooks run beside the
// user's). lever-agent boot writes the agent's Claude here, at every
// start, from the envelope the host staged.
//
// Why here and not in ~/.claude/settings.json: the home is the agent's own
// (uid 1000 owns it, and scion's harness writes its hooks there), while
// /etc/claude-code is root's. Boot runs as root in scion's pre-start hook;
// claude runs as the scion user, which can read this file but not change
// it. An agent that gains root in its own container can still rewrite it,
// and boot rewrites it again at the next start.
const ManagedSettingsPath = "/etc/claude-code/managed-settings.json"

// AutoCompactWindowEnv is the Claude Code variable Claude's
// AutoCompactWindow becomes. Claude Code ranks it above the
// autoCompactWindow setting, so neither /autocompact nor a settings file
// can override it in the session.
const AutoCompactWindowEnv = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"

// AfterCompactHookPrefix starts the command of lever's after-compaction hook.
// The note follows it base64-encoded, so the shell that runs the hook sees
// only [A-Za-z0-9+/=]. It is also how a re-run of boot finds lever's own hook
// among the others to replace it.
const AfterCompactHookPrefix = "lever-agent after-compact --note-b64 "
