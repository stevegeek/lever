package host

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// Terminal modes the attach's tmux client turns on and only turns off when it
// exits normally.
const (
	// Mouse reporting (1000/1002/1003) and its encodings (1005/1006/1015).
	attachResetMouse = "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l"
	// Focus events, bracketed paste, colour-scheme reports, and cursor and
	// keypad application modes (arrow keys sending ESC O A).
	attachResetInput = "\x1b[?1004l\x1b[?2004l\x1b[?2031l\x1b[?1l\x1b>"
	// xterm modifyOtherKeys and the current kitty keyboard flags set to 0. The
	// flags are set rather than popped so that an entry an enclosing program
	// pushed stays. A terminal without these discards them as unknown CSI
	// sequences; one that misreads the first as SGR 4;0 is reset by the SGR 0
	// below.
	attachResetKeys = "\x1b[>4;0m\x1b[=0;1u"
	// Full scroll region, then leave the alternate screen. 1049 also restores
	// the cursor saved when the screen was entered, and xterm, VTE and tmux do
	// that even when the alternate screen is already off. The DECSC first makes
	// terminals with a saved cursor per screen (xterm, VTE, kitty) keep the cursor
	// where it is then; a host tmux keeps the 1049 cursor apart and may move it.
	// The region reset homes the cursor, so it comes after the DECSC.
	attachResetAltScreen = "\x1b7\x1b[r\x1b[?1049l"
	// Cursor shown, ASCII character set, attributes reset.
	attachResetCursor = "\x1b[?25h\x1b(B\x1b[0m"
)

// attachResetSeq is what lever writes to the operator's terminal after an
// attach. abnormal: the attach's tmux client may not have run its own cleanup.
// The keys reset comes before the alternate-screen exit, so that it acts on
// the screen the client left on.
func attachResetSeq(abnormal bool) string {
	s := attachResetMouse + attachResetInput + attachResetKeys
	if abnormal {
		s += attachResetAltScreen
	}
	s += attachResetCursor
	if abnormal {
		return s + "\r\n"
	}
	// A clean tmux exit leaves the cursor at the start of a fresh line.
	return s + "\r"
}

// attachTerm is the operator's terminal around an attach: the line settings to
// put back, and where to write the mode resets. Only fixed bytes are ever
// written to it.
type attachTerm struct {
	in    *os.File
	saved *unix.Termios
	out   *os.File
}

// saveAttachTerm records in's line settings when it is a terminal, and keeps
// out only when it is a terminal: a script's or CI's output gets no escapes.
func saveAttachTerm(in, out *os.File) *attachTerm {
	t := &attachTerm{}
	if in != nil {
		if tio, err := unix.IoctlGetTermios(int(in.Fd()), ioctlGetTermios); err == nil {
			t.in, t.saved = in, tio
		}
	}
	if out != nil && isTerminal(out) {
		t.out = out
	}
	return t
}

// restore puts back the saved line settings and writes the mode resets. After
// an abnormal end the terminal's input queue is dropped: it holds the mouse and
// focus reports meant for the dead client, which the shell would echo. Errors
// are ignored: a hung-up terminal has nothing left to restore.
func (t *attachTerm) restore(abnormal bool) {
	if t.saved != nil && ttyForeground(t.in) {
		req := uint(ioctlSetTermios)
		if abnormal {
			req = ioctlSetTermiosFlush
		}
		_ = unix.IoctlSetTermios(int(t.in.Fd()), req, t.saved)
	}
	if t.out != nil && ttyForeground(t.out) {
		_, _ = t.out.WriteString(attachResetSeq(abnormal))
	}
}

// ttyForeground: setting or (under tostop) writing the controlling terminal
// from a background process group stops lever with SIGTTOU, so a backgrounded
// lever leaves the terminal alone. A terminal that is not lever's controlling
// terminal has no such rule.
func ttyForeground(f *os.File) bool {
	pgrp, err := unix.IoctlGetInt(int(f.Fd()), unix.TIOCGPGRP)
	if err != nil {
		return true
	}
	return pgrp == unix.Getpgrp()
}

// attachEndedAbnormally: only exit 0 shows the tmux client ran its cleanup. A
// client whose server died exits 1 with its modes still on, and that status is
// the agent's to choose.
func attachEndedAbnormally(err error) bool {
	return err != nil
}

// attachExitCode is the code a shell reports for the attach's end: its exit
// status, or 128+n when signal n killed it.
func attachExitCode(err error) (int, bool) {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return 0, false
	}
	if code := ee.ExitCode(); code >= 0 {
		return code, true
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), true
	}
	return 1, true
}
