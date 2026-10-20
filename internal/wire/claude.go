package wire

import (
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

// Claude is the per-agent Claude Code configuration the host stages in the
// agent's bootstrap envelope. lever-agent boot writes it into the
// managed-settings file at every start, a resume included.
//
// Boot validates it again after the host does: the manager's envelope sits in
// a tree the manager can write.
type Claude struct {
	// AutoCompactWindow becomes CLAUDE_CODE_AUTO_COMPACT_WINDOW: the context
	// size, in tokens, at which Claude Code compacts. 0 means unset.
	AutoCompactWindow int `json:"auto_compact_window,omitempty"`
	// AfterCompactNote is the operator's note Claude Code receives as
	// SessionStart context right after a compaction. "" means unset.
	AfterCompactNote string `json:"after_compact_note,omitempty"`
}

const (
	MinAutoCompactWindow     = 100_000
	MaxAutoCompactWindow     = 1_000_000
	MaxAfterCompactNoteBytes = 300
)

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
// valid UTF-8, and no control or format character (newline, bidi override),
// so the note stays one plain line of the agent's context. "" is valid.
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
// highest-precedence scope, merged over ~/.claude/settings.json (its env keys
// win, its hooks run beside the user's). It ranks above the agent's own
// settings files, which scion's harness also writes into.
//
// It is not a boundary: under rootless podman boot runs as the agent user in
// scion's pre-start hook and owns /etc/claude-code, so the agent can change
// the file during a session; boot rewrites it at the next start.
const ManagedSettingsPath = "/etc/claude-code/managed-settings.json"

// AutoCompactWindowEnv is the variable AutoCompactWindow becomes. Claude Code
// ranks it above the autoCompactWindow setting, so neither /autocompact nor a
// settings file can override it in the session.
const AutoCompactWindowEnv = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"

// AfterCompactHookPrefix starts the command of lever's after-compaction hook.
// The note follows base64-encoded, so the shell that runs the hook sees only
// [A-Za-z0-9+/=]. Boot also uses the prefix to find and replace its own hook.
const AfterCompactHookPrefix = "lever-agent after-compact --note-b64 "

// DisableAgentViewEnv and DisableAgentViewSetting turn off Claude Code's
// background agents and agent view; Claude Code documents them as
// equivalent. Boot always writes both; it is not an operator option.
//
// A backgrounded session leaves records in the agent home naming its
// container. After a container restart claude --continue cannot tell whether
// that session still runs, refuses and exits, so the agent dies at start.
const (
	DisableAgentViewEnv     = "CLAUDE_CODE_DISABLE_AGENT_VIEW"
	DisableAgentViewSetting = "disableAgentView"
)

// ShellPrefixEnv is Claude Code's CLAUDE_CODE_SHELL_PREFIX: a command every
// Bash tool command, hook, status line and stdio MCP server is run through,
// with the command line in $1. Boot points it at ToolShellPath when the image
// has that file (T0312), so the agent's own work can't starve claude of
// processes or memory. An image without it (an instance image built from an
// older Dockerfile) gets no prefix: a missing prefix would fail every command.
const (
	ShellPrefixEnv = "CLAUDE_CODE_SHELL_PREFIX"
	ToolShellPath  = "/usr/local/libexec/lever-toolshell"
)

// ReadyHookCommand is the command of lever's claude-ready SessionStart hook,
// which writes ReadyMarkerPath once a session (new or resumed) has started.
// The manager gate waits for that marker. Boot uses the command to find and
// replace its hook, and the gate uses it to tell an image that writes no
// marker from a claude that has not started yet.
const ReadyHookCommand = "lever-agent claude-ready"

// ReadyMarkerPath is in /tmp, not the agent home: the home outlives the
// container, and the marker means nothing outside the run that wrote it.
const ReadyMarkerPath = "/tmp/lever-claude-ready"

// ReadyMarkerMax bounds a read of the marker.
const ReadyMarkerMax = 256

// ReadyMarker is the marker's content for one claude session start: the
// guest kernel's boot id, the start time of the container's PID 1 (field 22
// of /proc/1/stat) and claude's tmux pane id. The first two name the
// container run, so a stale marker never matches. The reader builds the same
// line itself and compares for equality: the marker carries no data of its
// own.
func ReadyMarker(bootID, pid1Start, pane string) string {
	return "v1 " + bootID + " " + pid1Start + " " + pane
}
