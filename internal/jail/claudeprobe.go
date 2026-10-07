package jail

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/wire"
)

// ClaudeDelivered is what lever reads back of the Claude config boot wrote
// into an agent's managed-settings file (wire.ManagedSettingsPath). It is
// what the agent's claude got at its last start, unless the agent became
// root and rewrote the file since.
type ClaudeDelivered struct {
	// AutoCompactWindow is the file's CLAUDE_CODE_AUTO_COMPACT_WINDOW; 0 when
	// the file or the key is absent.
	AutoCompactWindow int
	// NoteSum is the SHA-256 of the note in lever's after-compaction hook;
	// zero when there is no such hook. Only the sum leaves the probe, never
	// the text: compare it with NoteSum(the configured note).
	NoteSum [32]byte
}

// NoteSum is the sum ClaudeDelivered.NoteSum holds for note ("" ⇒ zero).
func NoteSum(note string) [32]byte {
	if note == "" {
		return [32]byte{}
	}
	return sha256.Sum256([]byte(note))
}

// ErrClaudeShape is a managed-settings file lever cannot read: not JSON, or
// a window that is not a decimal integer. It names no part of the file.
var ErrClaudeShape = errors.New("reading agent claude settings: unexpected output (not JSON, or a window that is not an integer)")

// claudeAbsentExit is the exit code of claudeSettingsScript when the file
// does not exist (head would exit 1 for any failure).
const claudeAbsentExit = 3

// claudeSettingsScript prints the managed-settings file named by $1,
// bounded, or exits claudeAbsentExit when it does not exist.
var claudeSettingsScript = `[ -e "$1" ] || exit ` + strconv.Itoa(claudeAbsentExit) + `; head -c 32768 "$1"`

// ClaudeSettings reads back the Claude config in the agent's container. The
// file is the agent's word like every probe's output: only an int and a sum
// come back, and no error repeats the file. An absent file is a zero
// ClaudeDelivered, not an error. ErrNoContainer when podman knows no such
// container.
func (p AgentProbe) ClaudeSettings(ctx context.Context, ref string) (ClaudeDelivered, error) {
	if err := checkRef(ref); err != nil {
		return ClaudeDelivered{}, fmt.Errorf("reading agent claude settings: %w", err)
	}
	ctx, cancel := BoundAgentExec(ctx)
	defer cancel()
	res, err := p.R.Run(ctx, nil, "podman", "exec", "--user", AgentUser, ref, "sh", "-c", claudeSettingsScript, "sh", wire.ManagedSettingsPath)
	if err != nil {
		switch {
		case noSuchContainer(res.Stderr):
			return ClaudeDelivered{}, fmt.Errorf("reading agent claude settings in %s: %w", ref, ErrNoContainer)
		case errors.Is(err, proc.ErrOutputLimit):
			return ClaudeDelivered{}, fmt.Errorf("reading agent claude settings in %s: %w", ref, proc.ErrOutputLimit)
		case ctx.Err() != nil:
			return ClaudeDelivered{}, fmt.Errorf("reading agent claude settings in %s: %w", ref, ErrProbeTimedOut)
		case res.Code == claudeAbsentExit:
			return ClaudeDelivered{}, nil
		}
		return ClaudeDelivered{}, fmt.Errorf("reading agent claude settings in %s: %w", ref, &ProbeExitError{Code: res.Code})
	}
	return parseClaudeDelivered(res.Stdout)
}

// parseClaudeDelivered reads the window and the sum of lever's note out of
// a managed-settings file. A window outside wire's bounds still parses: the
// caller compares it with the config, which is the finding.
func parseClaudeDelivered(out string) (ClaudeDelivered, error) {
	var f struct {
		Env   map[string]any `json:"env"`
		Hooks struct {
			SessionStart []struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"SessionStart"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(out), &f); err != nil {
		return ClaudeDelivered{}, ErrClaudeShape
	}
	var d ClaudeDelivered
	if v, ok := f.Env[wire.AutoCompactWindowEnv]; ok {
		s, _ := v.(string)
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 0 {
			return ClaudeDelivered{}, ErrClaudeShape
		}
		d.AutoCompactWindow = n
	}
	for _, g := range f.Hooks.SessionStart {
		for _, h := range g.Hooks {
			rest, ok := strings.CutPrefix(h.Command, wire.AfterCompactHookPrefix)
			if !ok {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(rest)
			if err != nil || len(raw) == 0 {
				return ClaudeDelivered{}, ErrClaudeShape
			}
			d.NoteSum = sha256.Sum256(raw)
		}
	}
	return d, nil
}

// Matches reports whether d is what boot writes for c (nil c: nothing).
func (d ClaudeDelivered) Matches(c *wire.Claude) bool {
	var want wire.Claude
	if c != nil {
		want = *c
	}
	return d.AutoCompactWindow == want.AutoCompactWindow && d.NoteSum == NoteSum(want.AfterCompactNote)
}

// DescribeClaude names c in an operator line: "window 400000, after-compact
// note" or "none". It never quotes the note.
func DescribeClaude(c *wire.Claude) string {
	var parts []string
	if c != nil && c.AutoCompactWindow > 0 {
		parts = append(parts, "window "+strconv.Itoa(c.AutoCompactWindow))
	}
	if c != nil && c.AfterCompactNote != "" {
		parts = append(parts, "after-compact note")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// Describe names d like DescribeClaude: the window it holds and whether it
// holds a note (which one is not known, only its sum).
func (d ClaudeDelivered) Describe() string {
	var parts []string
	if d.AutoCompactWindow > 0 {
		parts = append(parts, "window "+strconv.Itoa(d.AutoCompactWindow))
	}
	if d.NoteSum != ([32]byte{}) {
		parts = append(parts, "after-compact note")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
