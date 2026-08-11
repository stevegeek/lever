package scion

import (
	"context"
	"strings"
	"testing"

	leverexec "github.com/stevegeek/lever/internal/exec"
)

// msgArgv runs one Message and returns the argv the runner saw.
func msgArgv(t *testing.T, o MsgOpts) []string {
	t.Helper()
	f := leverexec.NewFakeRunner()
	f.Script("scion message", leverexec.Result{})
	if err := New(f, Options{}).Message(context.Background(), o); err != nil {
		t.Fatalf("Message: %v", err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(f.Calls))
	}
	return append([]string{f.Calls[0].Name}, f.Calls[0].Args...)
}

// A message body is agent-controlled. scion's `message` command defines
// single-token boolean flags -b/--broadcast and -a/--all, and cobra parses flags
// interspersed with positionals, so an unterminated body of exactly "-b" would
// bind to the broadcast flag and fan the message out to every agent in the
// project — past the worker-to-worker deny the broker applied to the recipient.
func TestMessagePutsPositionalsBehindATerminator(t *testing.T) {
	for _, body := range []string{"-b", "--all", "-a", "--broadcast", "-i"} {
		got := msgArgv(t, MsgOpts{To: "agent:manager", Body: body, Project: "/lever"})
		argv := strings.Join(got, " ")
		sep := -1
		for i, a := range got {
			if a == "--" {
				sep = i
				break
			}
		}
		if sep < 0 {
			t.Fatalf("body %q: no `--` terminator in argv: %s", body, argv)
		}
		if len(got) != sep+3 || got[sep+1] != "agent:manager" || got[sep+2] != body {
			t.Fatalf("body %q: recipient and body must be the only args after `--`, got: %s", body, argv)
		}
	}
}

// The ordinary flags must still be flags — they precede the terminator.
func TestMessageKeepsItsOwnFlagsBeforeTheTerminator(t *testing.T) {
	argv := strings.Join(msgArgv(t, MsgOpts{To: "agent:w", Body: "hello", Interrupt: true, Project: "/lever"}), " ")
	if !strings.Contains(argv, "--interrupt") || !strings.Contains(argv, "-g /lever") {
		t.Fatalf("flags lost: %s", argv)
	}
	if strings.Index(argv, "--interrupt") > strings.Index(argv, " -- ") {
		t.Fatalf("--interrupt must precede the terminator: %s", argv)
	}
}
