package apply

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
)

func TestWarnManagerClaude(t *testing.T) {
	var logs []string
	var refs []string
	app := &config.App{Name: "assistant", Manager: config.Manager{
		ClaudeSettings: config.ClaudeSettings{AutoCompactWindow: 400000}, AfterCompactNote: "Re-read NOTES.md"}}
	r := &run{app: app, d: Deps{Log: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}}
	read := func(d jail.ClaudeDelivered, err error) func(context.Context, string) (jail.ClaudeDelivered, error) {
		return func(_ context.Context, ref string) (jail.ClaudeDelivered, error) {
			refs = append(refs, ref)
			return d, err
		}
	}

	// No reader: nothing to say.
	r.warnManagerClaude(context.Background(), "/lever/proj")
	if len(logs) != 0 {
		t.Fatalf("no reader: %q", logs)
	}

	r.d.ReadClaudeSettings = read(jail.ClaudeDelivered{AutoCompactWindow: 400000, NoteSum: jail.NoteSum("Re-read NOTES.md")}, nil)
	r.warnManagerClaude(context.Background(), "/lever/proj")
	if len(logs) != 0 || refs[0] != jail.ContainerName("proj", "assistant") {
		t.Fatalf("as configured: logs %q refs %q", logs, refs)
	}

	r.d.ReadClaudeSettings = read(jail.ClaudeDelivered{AutoCompactWindow: 200000}, nil)
	r.warnManagerClaude(context.Background(), "/lever/proj")
	if len(logs) != 1 || !strings.Contains(logs[0], "runs with claude settings window 200000, but the config sets window 400000, after-compact note") ||
		!strings.Contains(logs[0], "lever stop && lever up") || !strings.Contains(logs[0], "make lever-image") ||
		!strings.Contains(logs[0], "lever up --fresh") || !strings.Contains(logs[0], "back up its conversation") {
		t.Fatalf("differs: %q", logs)
	}

	logs = nil
	r.d.ReadClaudeSettings = read(jail.ClaudeDelivered{}, errors.New("boom"))
	r.warnManagerClaude(context.Background(), "/lever/proj")
	if len(logs) != 0 {
		t.Fatalf("a failed read is the doctor row's to report: %q", logs)
	}

	// Config removed while the manager still holds lever's values: warn.
	r.app = &config.App{Name: "assistant"}
	r.d.ReadClaudeSettings = read(jail.ClaudeDelivered{AutoCompactWindow: 400000}, nil)
	r.warnManagerClaude(context.Background(), "/lever/proj")
	if len(logs) != 1 || !strings.Contains(logs[0], "runs with claude settings window 400000, but the config sets none") {
		t.Fatalf("removed: %q", logs)
	}
	logs = nil

	// Nothing configured, nothing delivered: silent.
	r.d.ReadClaudeSettings = read(jail.ClaudeDelivered{}, nil)
	r.warnManagerClaude(context.Background(), "/lever/proj")
	if len(logs) != 0 {
		t.Fatalf("unset: %q", logs)
	}
}
