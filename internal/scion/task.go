package scion

import (
	"fmt"
	"strings"
)

// The agent TASK rides scion's argv: `scion start <agent> <task>` becomes a
// harness command line that scion single-quotes and hands to `tmux
// new-session` as ONE word, inside a `sh -c '…; echo $? > file'` wrapper.
// tmux's client sends the whole command to its server in a single imsg
// message capped at 16384 bytes (MAX_IMSGSIZE, a compile-time constant that
// has held for years), so a task past roughly 16 KiB never starts its agent:
// the container exits 1 and the only trace is "command too long" (or "failed
// to send command") in its log (lever#30). Three layers pass the task along
// and only the last one has a cap, which is why lever checks the size itself,
// up front, by name.
//
// Budget derivation, MEASURED against the claude harness at the pinned scion
// by replaying scion's exact construction through a real `sh -c` and summing
// len(arg)+1 over the argv tmux receives — the sum tmux compares to the cap:
// tmux 3.6b accepts 16362 and refuses 16365. The fixed part — the five-verb
// tmux chain, the `sh -c` wrapper, claude's three base args, the task's own
// two quotes and the exit-code redirection — costs 261 bytes, leaving 16101
// for the task itself. TaskArgvBudget keeps 741 of those in reserve as slack
// against harness-arg drift (a renamed flag, an extra base arg), so the check
// stays on the safe side of the cliff rather than on it.
//
// The check covers the TASK only. A scion template that sets `system_prompt`
// puts its own text on the same command line, uncounted; lever's overlay
// template keeps that empty (internal/backend/guest/template.go), so nothing
// lever deploys today adds to the sum.
//
// Standing instructions do not belong here at all: StartOpts.Instructions
// delivers them as a file through scion's agent_instructions channel, which
// has no limit of this kind (CheckInstructions holds the one it does have).
const TaskArgvBudget = 15 * 1024

// apostropheArgvCost is what one apostrophe adds beyond its own byte by the
// time tmux sees it: scion's shellQuote rewrites `'` as `'\”`, four bytes.
// scion does quote the whole wrapper a second time when it builds the tmux
// command STRING, but that string is run through the container's own `sh -c`
// (pkg/runtime/common.go), which strips the outer pass before tmux parses a
// single word — so only the first pass counts. Measured: ten apostrophes add
// 40 bytes to the argv sum, ten plain characters add 10.
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

// CheckTask returns a *TaskTooLongError when task cannot start an agent, nil
// otherwise. Pure and cheap: call it as early as the task is known (config
// load, request decode) so the failure is a named error at the seam that owns
// the text, and Start calls it again as the backstop for every path.
func CheckTask(task string) error {
	if n := TaskArgvBytes(task); n > TaskArgvBudget {
		return &TaskTooLongError{Bytes: n, Budget: TaskArgvBudget}
	}
	return nil
}

// MaxInstructionsBytes caps StartOpts.Instructions. The text travels inside
// scion's hub create request, and the hub↔runtime-broker control channel
// caps one message at 1 MiB (scion pkg/wsprotocol DefaultMaxMessageSize);
// half of that leaves room for the rest of the request and is still far
// beyond any operating manual. Named here because the alternative is an
// opaque transport error several layers below the config key that caused it.
const MaxInstructionsBytes = 512 * 1024

// instructionsURIPrefix: scion resolves the agent_instructions value through
// api.ResolveContent BEFORE provisioning, and a value that begins with this
// prefix is read as a file reference (absolute for file:///, else relative to
// the scion process's cwd in the jail) instead of being staged as text.
const instructionsURIPrefix = "file://"

// CheckInstructions returns a named error when text cannot be sent as an
// agent's standing instructions: over MaxInstructionsBytes, or beginning with
// file://, which scion would silently reinterpret as a path. Empty passes —
// it means "send no config at all" (see StartOpts.Instructions).
func CheckInstructions(text string) error {
	if n := len(text); n > MaxInstructionsBytes {
		return fmt.Errorf("agent instructions are %d bytes, over lever's %d-byte cap (they travel inside a scion hub request capped at 1 MiB)", n, MaxInstructionsBytes)
	}
	if strings.HasPrefix(text, instructionsURIPrefix) {
		return fmt.Errorf("agent instructions must not begin with %q: scion reads such a value as a file reference, not as text", instructionsURIPrefix)
	}
	return nil
}
