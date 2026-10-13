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
// Why here and not in ~/.claude/settings.json: managed settings rank above
// the agent's own settings files, and scion's harness writes its hooks in
// the home. It is not a boundary: under rootless podman boot runs as the
// agent user (uid 1000) in scion's pre-start hook, and lever's image gives
// that user /etc/claude-code, so the agent can change the file during a
// session; boot rewrites it at the next start, unless it cannot (see the
// boot log).
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

// DisableAgentViewEnv and DisableAgentViewSetting turn off Claude Code's
// background agents and agent view (claude agents, --bg, /background, the
// left-arrow shortcut that backgrounds the session, and the on-demand
// supervisor). Claude Code documents the setting as equivalent to the
// variable. lever-agent boot writes both into the managed-settings file of
// every agent at every start; it is not an operator option.
//
// Why: a session moved into the background registers session records in the
// agent home that name the container it ran in. After a container restart
// the next claude --continue cannot tell whether that background session
// still runs, refuses, and exits, so the agent dies at start.
const (
	DisableAgentViewEnv     = "CLAUDE_CODE_DISABLE_AGENT_VIEW"
	DisableAgentViewSetting = "disableAgentView"
)

// ReadyHookCommand is the command of lever's claude-ready hook: a
// SessionStart hook with no matcher that lever-agent boot writes into the
// managed-settings file of every agent at every start. Claude Code runs it
// once a session has started (a new one, or the one --continue resumed), and
// it writes ReadyMarkerPath. lever's manager gate waits for that marker after
// a start (lever#188); the command is also how a re-run of boot finds the
// hook to replace it, and how the gate tells an image that writes no marker
// (an older lever-agent) from a claude that has not started yet.
const ReadyHookCommand = "lever-agent claude-ready"

// ReadyMarkerPath is where the claude-ready hook writes its marker inside
// the container. /tmp, not the agent home: the home outlives the container,
// and the marker means nothing outside the container run that wrote it.
const ReadyMarkerPath = "/tmp/lever-claude-ready"

// ReadyMarkerMax bounds a read of the marker.
const ReadyMarkerMax = 256

// ReadyMarker is the marker's content for one claude session start: the
// guest kernel's boot id, the start time of the container's PID 1 (field 22
// of /proc/1/stat, in clock ticks since boot) and claude's tmux pane id
// ($TMUX_PANE). The first two name the container run, so a marker a
// previous run left behind never matches; the pane id ties it to the pane
// scion runs claude in. The reader builds the same line from what it reads
// itself and compares the two for equality: the marker carries no data of
// its own.
func ReadyMarker(bootID, pid1Start, pane string) string {
	return "v1 " + bootID + " " + pid1Start + " " + pane
}
