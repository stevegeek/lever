package host

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/cli"
)

// A fake attach for each way an attach ends: a detach, a client whose server
// died, a lost connection, and a killed client.
var fakeAttachEnds = []struct {
	name     string
	script   string
	code     int
	abnormal bool
}{
	{"exit 0", "exit 0", 0, false},
	{"exit 1", "exit 1", 1, true},
	{"exit 255", "exit 255", 255, true},
	{"killed", "kill -KILL $$", 128 + 9, true},
}

func TestAttachEndAndExitCode(t *testing.T) {
	for _, tc := range fakeAttachEnds {
		t.Run(tc.name, func(t *testing.T) {
			err := attachChild([]string{"sh", "-c", tc.script}, nil, nil, nil)
			if got := attachEndedAbnormally(err); got != tc.abnormal {
				t.Errorf("abnormal = %t, want %t (err %v)", got, tc.abnormal, err)
			}
			werr := withAttachExitCode(err)
			if tc.code == 0 {
				if werr != nil {
					t.Fatalf("err = %v, want nil", werr)
				}
				return
			}
			var ee *cli.ExitError
			if !errors.As(werr, &ee) || ee.Code != tc.code {
				t.Fatalf("err = %v, want exit code %d", werr, tc.code)
			}
		})
	}
}

// A lookup or start failure is not an attach that ran: no exit code of its own.
func TestAttachExitCodeOnlyForARunAttach(t *testing.T) {
	err := attachChild([]string{"lever-no-such-attach-binary"}, nil, nil, nil)
	if err == nil {
		t.Fatal("want a lookup error")
	}
	var ee *cli.ExitError
	if errors.As(withAttachExitCode(err), &ee) {
		t.Errorf("lookup failure got exit code %d", ee.Code)
	}
}

// Off a terminal (a script, CI) lever writes nothing of its own after the
// attach.
func TestAttachWritesNothingOffATerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_ = attachChild([]string{"sh", "-c", "printf attach-output; exit 255"}, nil, w, w)
	w.Close()
	got, _ := io.ReadAll(r)
	if string(got) != "attach-output" {
		t.Errorf("output %q, want only the attach's own", got)
	}
}

func TestAttachResetSeq(t *testing.T) {
	clean, abnormal := attachResetSeq(false), attachResetSeq(true)
	for _, mode := range []string{"\x1b[?1000l", "\x1b[?1002l", "\x1b[?1003l", "\x1b[?1006l", "\x1b[?1004l", "\x1b[?2004l", "\x1b[=0;1u", "\x1b[?1l\x1b>", "\x1b[?2031l", "\x1b[?25h"} {
		if !strings.Contains(clean, mode) || !strings.Contains(abnormal, mode) {
			t.Errorf("%q missing", mode)
		}
	}
	// SGR 0 comes after the modifyOtherKeys reset that some terminals misread as
	// SGR 4;0.
	if strings.Index(clean, "\x1b[0m") < strings.Index(clean, "\x1b[>4;0m") {
		t.Error("SGR 0 must follow the modifyOtherKeys reset")
	}
	if strings.Contains(clean, "1049") || !strings.HasSuffix(clean, "\r") || strings.HasSuffix(clean, "\n") {
		t.Errorf("clean end %q: want no alternate-screen exit and a bare carriage return", clean)
	}
	if !strings.Contains(abnormal, "\x1b7\x1b[r\x1b[?1049l") || !strings.HasSuffix(abnormal, "\r\n") {
		t.Errorf("abnormal end %q: want the alternate-screen exit and a new line", abnormal)
	}
}

// The attach runs with exactly the argv it was given.
func TestAttachChildArgvIsTheAttachArgv(t *testing.T) {
	dir := t.TempDir()
	out := dir + "/argv"
	err := attachChild([]string{"sh", "-c", `printf '%s|' "$0" "$@" > "$1"`, "zero", out, "x y"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "zero|"+out+"|x y|" {
		t.Errorf("argv %q", got)
	}
}
