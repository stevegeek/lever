package apply

import "strings"

// sciontool's reaper logs every orphaned child it reaps, the agent
// container's tmux server among them (scion pkg/sciontool/procreap):
//
//	Reaped zombie process 82 (tmux: server) (reason: killed by signal segmentation fault, exit code: -1)
//
// The tmux server holds claude's pane, so its death takes the harness with
// it (lever#201: tmux 3.5a segfaulted about 2.5 minutes into a manager's
// run). tmuxServerReapedPrefix is the start of that line for the server,
// squashed as HarnessDeathHint squashes the tail; the signal follows.
const tmuxServerReapedPrefix = "(tmux:server)(reason:killedbysignal"

// tmuxCrashSignals are the signal names (Go's syscall.Signal strings, as
// sciontool prints them) that mean the tmux server crashed: a fault or an
// abort in tmux itself. Squashed like the tail.
var tmuxCrashSignals = []string{
	"segmentationfault",
	"aborted",
	"buserror",
	"illegalinstruction",
	"floatingpointexception",
}

// TmuxServerCrashHint is the cause lever prints when the agent container's
// log says sciontool reaped its tmux server after a crash signal.
const TmuxServerCrashHint = "cause: the tmux server in the agent's container crashed (sciontool reaped it after a " +
	"fault signal such as a segmentation fault), and claude's pane went with it. This is a tmux bug (lever#201), " +
	"not a claude or lever failure. `lever up` starts the manager again; the conversation is kept when it had " +
	"one. In a Lima jail the guest keeps a core of the crashed tmux server in the guest (root only, " +
	"/var/lib/lever/cores) when the kernel could write one; see the operations guide, \"tmux server crashes\""

// TmuxServerKilledHint is the cause lever prints when sciontool reaped the
// tmux server after SIGKILL: not a tmux bug, and most often the guest's
// out-of-memory killer.
const TmuxServerKilledHint = "cause: the tmux server in the agent's container was killed (SIGKILL, as sciontool " +
	"logged it), and claude's pane went with it. The guest's out-of-memory killer is the usual sender: its " +
	"kernel log (sudo dmesg in the guest) names the victim. `lever up` starts the manager again; the conversation " +
	"is kept when it had one"

// tmuxServerDeathHint returns the hint for a tmux server death that
// sciontool logged in norm (a tail squashed by squashForMatch), or "" when
// the tail names none. Only fixed strings are returned.
func tmuxServerDeathHint(norm string) string {
	for rest := norm; ; {
		i := strings.Index(rest, tmuxServerReapedPrefix)
		if i < 0 {
			return ""
		}
		rest = rest[i+len(tmuxServerReapedPrefix):]
		for _, s := range tmuxCrashSignals {
			if strings.HasPrefix(rest, s) {
				return TmuxServerCrashHint
			}
		}
		if strings.HasPrefix(rest, "killed,") {
			return TmuxServerKilledHint
		}
	}
}
