package apply

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/scion"
)

// The line sciontool logged when the agent's tmux 3.5a server segfaulted
// (lever#201), and around it what the container's own tmux client printed.
const tmuxSegfaultLog = "\x1b[?2026h\x1b[2J\x1b[3J\x1b[H Opus 5.5 · Claude API\r\n[lost server]\r\n" +
	"2026-10-12 17:15:22 [sciontool] INFO Reaped zombie process 82 (tmux: server) (reason: killed by signal segmentation fault, exit code: -1)\n" +
	"2026-10-12 17:15:22 [sciontool] INFO Reported final status to Hub (exitCode=1, crash=true)\n"

func TestHarnessDeathHintTmuxServer(t *testing.T) {
	for _, c := range []struct {
		name string
		tail string
		want string
	}{
		{"segfault", tmuxSegfaultLog, TmuxServerCrashHint},
		{"abort", "Reaped zombie process 7 (tmux: server) (reason: killed by signal aborted, exit code: -1)", TmuxServerCrashHint},
		{"bus error", "Reaped zombie process 7 (tmux: server) (reason: killed by signal bus error, exit code: -1)", TmuxServerCrashHint},
		{"sigkill", "Reaped zombie process 7 (tmux: server) (reason: killed by signal killed, exit code: -1)", TmuxServerKilledHint},
		{"wrapped by the pane", "Reaped zombie process 7 (tmux:\r\n server) (reason: killed by\x1b[1Csignal segmentation\r\nfault, exit code: -1)", TmuxServerCrashHint},
		{"second line names the crash", "Reaped zombie process 7 (tmux: server) (reason: killed by signal terminated, exit code: -1)\nReaped zombie process 9 (tmux: server) (reason: killed by signal segmentation fault, exit code: -1)", TmuxServerCrashHint},
		{"last line wins", "Reaped zombie process 7 (tmux: server) (reason: killed by signal segmentation fault, exit code: -1)\nReaped zombie process 9 (tmux: server) (reason: killed by signal killed, exit code: -1)", TmuxServerKilledHint},
		{"a later clean exit clears it", "Reaped zombie process 7 (tmux: server) (reason: killed by signal segmentation fault, exit code: -1)\nReaped zombie process 9 (tmux: server) (reason: exited, exit code: 0)", ""},
		{"sigtrap", "Reaped zombie process 7 (tmux: server) (reason: killed by signal trace/breakpoint trap, exit code: -1)", TmuxServerCrashHint},
		{"clean exit", "Reaped zombie process 7 (tmux: server) (reason: exited, exit code: 0)", ""},
		{"sigterm", "Reaped zombie process 7 (tmux: server) (reason: killed by signal terminated, exit code: -1)", ""},
		{"another process segfaulted", "Reaped zombie process 7 (node) (reason: killed by signal segmentation fault, exit code: -1)", ""},
		{"a client, not the server", "Reaped zombie process 7 (tmux: client) (reason: killed by signal segmentation fault, exit code: -1)", ""},
		{"lost server alone", "[lost server]", ""},
	} {
		if got := HarnessDeathHint(c.tail); got != c.want {
			t.Errorf("%s: hint = %q, want %q", c.name, got, c.want)
		}
	}
	for _, w := range []string{"tmux server", "tmux crash", "not proof", "lever up", "/var/lib/lever/cores", "tmux server crashes"} {
		if !strings.Contains(TmuxServerCrashHint, w) {
			t.Errorf("crash hint lacks %q", w)
		}
	}
	for _, w := range []string{"SIGKILL", "out-of-memory", "dmesg", "lever up"} {
		if !strings.Contains(TmuxServerKilledHint, w) {
			t.Errorf("killed hint lacks %q", w)
		}
	}
}

// A stale background session is the more specific cause when the log names
// both: it is what blocks the next start.
func TestHarnessDeathHintBackgroundBeforeTmuxCrash(t *testing.T) {
	if got := HarnessDeathHint(tmuxSegfaultLog + staleBgLog); got != StaleBackgroundHint {
		t.Fatalf("hint = %q, want the stale background hint", got)
	}
}

// A liveness failure (the start gate, or lever up after the attach) names a
// tmux server crash the container log shows, and repeats nothing of the log.
func TestWithHarnessDeathHintNamesTmuxServerCrash(t *testing.T) {
	deps := Deps{
		HarnessLogTail: func(context.Context, string) (string, error) {
			return "SECRET-LINE\n" + tmuxSegfaultLog, nil
		},
	}
	err := withHarnessDeathHint(context.Background(), deps, scion.ErrAgentDied, "/p/proj", "mgr")
	if !errors.Is(err, scion.ErrAgentDied) || !strings.Contains(err.Error(), TmuxServerCrashHint) {
		t.Fatalf("want ErrAgentDied with the tmux crash hint, got %v", err)
	}
	if strings.Contains(err.Error(), "SECRET-LINE") {
		t.Fatalf("error repeats the container log: %v", err)
	}
}
