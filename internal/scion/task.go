package scion

import (
	"fmt"
	"strings"
)

// TaskArgvBudget bounds the task on scion's tmux command line. scion start
// hands the task to tmux new-session as one word, and tmux's client sends the
// whole command in a single imsg capped at 16384 bytes (MAX_IMSGSIZE); a task
// past that never starts its agent, and the only trace is "command too long"
// in the container log.
//
// Measured against the claude harness at the pinned scion (tmux 3.6b), the
// fixed part of the command costs 261 bytes, leaving 16101 for the task; the
// budget keeps 741 in reserve against harness-arg drift.
//
// It covers the task only: a template system_prompt would add uncounted text,
// so lever's overlay template keeps it empty. Standing instructions travel as
// a file instead (see CheckInstructions).
const TaskArgvBudget = 15 * 1024

// apostropheArgvCost is what one apostrophe adds beyond its own byte: scion's
// shellQuote rewrites each as four bytes (quote, backslash, quote, quote).
// scion's second quoting pass is stripped by the container's sh -c before
// tmux sees it, so only the first counts.
const apostropheArgvCost = 3

// TaskArgvBytes is the size the task occupies on scion's tmux command line:
// its bytes plus the expansion of every apostrophe under scion's quoting.
// Fixed overhead is accounted for in TaskArgvBudget, not here.
func TaskArgvBytes(task string) int {
	return len(task) + apostropheArgvCost*strings.Count(task, "'")
}

// TaskTooLongError reports a task that cannot start its agent because it
// would push scion's tmux command past the imsg cap. Bytes is TaskArgvBytes
// of the task; Budget is TaskArgvBudget.
type TaskTooLongError struct {
	Bytes  int
	Budget int
}

func (e *TaskTooLongError) Error() string {
	return fmt.Sprintf("agent task is %d argv bytes (its size plus %d per apostrophe), over lever's %d-byte budget: "+
		"scion passes the task on the command line, where tmux caps the whole start command at 16 KiB, "+
		"so the container would exit with \"command too long\" before the agent ran; "+
		"move standing instructions into instructions_file and keep the task a short first turn",
		e.Bytes, apostropheArgvCost, e.Budget)
}

// TaskFlagError reports a flag-shaped task (see flagShaped). scion start
// parses interspersed flags, so such a task would bind as one of scion's own
// flags (--config=<path> authors the agent's inline config). Client.Start
// also places the task behind --; this is the second layer.
type TaskFlagError struct {
	Prefix string
}

func (e *TaskFlagError) Error() string {
	return fmt.Sprintf("agent task begins with %q: a task is never a flag, and scion would read it as one; start the task with a word", e.Prefix)
}

// CheckTask returns a *TaskTooLongError when task cannot start an agent and a
// *TaskFlagError when it is flag-shaped; nil otherwise. Call it as early as
// the task is known; Start calls it again as the backstop.
func CheckTask(task string) error {
	if n := TaskArgvBytes(task); n > TaskArgvBudget {
		return &TaskTooLongError{Bytes: n, Budget: TaskArgvBudget}
	}
	if flagShaped(task) {
		prefix, _, _ := strings.Cut(task, " ")
		return &TaskFlagError{Prefix: prefix}
	}
	return nil
}

// flagShaped reports whether task's first token would parse as a flag: one or
// two dashes followed by a letter, or a token made only of dashes. A markdown
// list item ("- do x") or a negative number is a task.
func flagShaped(task string) bool {
	tok, _, _ := strings.Cut(task, " ")
	if tok == "" || tok[0] != '-' {
		return false
	}
	rest := strings.TrimLeft(tok, "-")
	if rest == "" {
		return tok == task
	}
	c := rest[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// MaxInstructionsBytes caps StartOpts.Instructions. The text travels inside
// scion's hub create request, and the hub-to-runtime-broker channel caps one
// message at 1 MiB (pkg/wsprotocol DefaultMaxMessageSize); exceeding it would
// surface as an opaque transport error.
const MaxInstructionsBytes = 512 * 1024

// instructionsURIPrefix: scion resolves agent_instructions through
// api.ResolveContent, which reads a value with this prefix as a file
// reference in the jail instead of as text.
const instructionsURIPrefix = "file://"

// CheckInstructions returns a named error when text cannot be sent as an
// agent's standing instructions: over MaxInstructionsBytes, or beginning with
// file://, which scion would silently reinterpret as a path. Empty passes.
func CheckInstructions(text string) error {
	if n := len(text); n > MaxInstructionsBytes {
		return fmt.Errorf("agent instructions are %d bytes, over lever's %d-byte cap (they travel inside a scion hub request capped at 1 MiB)", n, MaxInstructionsBytes)
	}
	if strings.HasPrefix(text, instructionsURIPrefix) {
		return fmt.Errorf("agent instructions must not begin with %q: scion reads such a value as a file reference, not as text", instructionsURIPrefix)
	}
	return nil
}
