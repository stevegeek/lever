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
// squashed as HarnessDeathHint squashes the tail; the reason follows.
const tmuxServerReapedPrefix = "(tmux:server)(reason:"

// tmuxCrashSignals are the signal names (Go's syscall.Signal strings, as
// sciontool prints them) that mean the tmux server crashed: a fault or an
// abort in tmux itself. Squashed like the tail.
var tmuxCrashSignals = []string{
	"segmentationfault",
	"aborted",
	"buserror",
	"illegalinstruction",
	"floatingpointexception",
	"trace/breakpointtrap",
	"badsystemcall",
}

// TmuxServerCrashHint is the cause lever prints when the agent container's
// log says sciontool reaped its tmux server after a crash signal.
const TmuxServerCrashHint = "cause: the agent container's log reports that sciontool reaped its tmux server after a " +
	"fault signal such as a segmentation fault, and claude's pane went with it: usually a tmux crash (lever#201). " +
	"The log is the agent's word, so treat this as a lead, not proof. `lever up` starts the manager again; the conversation is kept when it had " +
	"one. In a Lima jail the guest keeps a core of a crashed tmux server (root only, " +
	"/var/lib/lever/cores) when the kernel could write one; see the operations guide, \"tmux server crashes\""

// TmuxServerKilledHint is the cause lever prints when sciontool reaped the
// tmux server after SIGKILL: not a tmux bug, and most often the guest's
// out-of-memory killer.
const TmuxServerKilledHint = "cause: the agent container's log reports that sciontool reaped its tmux server after " +
	"SIGKILL, and claude's pane went with it. The guest's out-of-memory killer is the usual sender: its " +
	"kernel log (sudo dmesg in the guest) names the victim. `lever up` starts the manager again; the conversation " +
	"is kept when it had one"

// tmuxServerDeathHint returns the hint for the last tmux server death that
// sciontool logged in norm (a tail squashed by squashForMatch), or "" when
// the tail names none: the last, because a tail can reach back into an
// earlier run of the same container. Only fixed strings are returned.
func tmuxServerDeathHint(norm string) string {
	hint := ""
	for rest := norm; ; {
		i := strings.Index(rest, tmuxServerReapedPrefix)
		if i < 0 {
			return hint
		}
		rest = rest[i+len(tmuxServerReapedPrefix):]
		sig, signalled := strings.CutPrefix(rest, "killedbysignal")
		switch {
		case signalled && hasAnyPrefix(sig, tmuxCrashSignals):
			hint = TmuxServerCrashHint
		case signalled && strings.HasPrefix(sig, "killed,"):
			hint = TmuxServerKilledHint
		default:
			hint = ""
		}
	}
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
