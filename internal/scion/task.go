package scion

import (
	"fmt"
	"strings"
)

// The agent TASK rides scion's argv: `scion start <agent> <task>` becomes a
// harness command line that scion single-quotes, wraps in `sh -c '…; echo $?
// > file'` (quoting it again), and hands to `tmux new-session` as ONE word.
// tmux's client sends that whole command to its server in a single imsg
// message capped at 16384 bytes (MAX_IMSGSIZE, a compile-time constant that
// has held for years), so a task past roughly 16 KiB never starts its agent:
// the container exits 1 and the only trace is "command too long" in its log
// (lever#30). Three layers pass the task along and only the last one has a
// cap, which is why lever checks the size itself, up front, by name.
//
// Budget derivation, measured against the claude harness at the pinned scion:
// tmux accepts a summed argv (len+1 per word) of 16364 bytes. The fixed part —
// the new-session/set-option/new-window/select-window/attach-session chain
// (156), the `sh -c` wrapper and outer quotes, claude's three base args, and
// the exit-code redirection — costs 287 bytes, leaving 16077 for the quoted
// task. TaskArgvBudget keeps 717 bytes of that in reserve for extra harness
// args (a template system prompt, a future flag) so the check stays on the
// safe side of the cliff rather than on it.
//
// Standing instructions do not belong here at all: StartOpts.Instructions
// delivers them as a file through scion's agent_instructions channel, which
// has no size limit of this kind.
const TaskArgvBudget = 15 * 1024

// apostropheArgvCost is what one apostrophe adds beyond its own byte once the
// task has been quoted twice: `'` → `'\”` (4 bytes) on the first pass, and
// the three quotes in that expand again on the second, to 13 bytes in all.
const apostropheArgvCost = 12

// TaskArgvBytes is the size the task occupies on scion's tmux command line:
// its bytes plus the expansion of every apostrophe under scion's two rounds of
// single-quoting. Fixed overhead is accounted for in TaskArgvBudget, not here.
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
