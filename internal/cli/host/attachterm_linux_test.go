package host

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openPty opens a pseudo-terminal pair without making it the controlling
// terminal.
func openPty(t *testing.T) (master, slave *os.File) {
	t.Helper()
	// Non-blocking, so the runtime poller honours read deadlines.
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	m := os.NewFile(uintptr(fd), "ptmx")
	t.Cleanup(func() { m.Close() })
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return m, s
}

// readPty reads what the slave side wrote: want bytes (within 2s), then
// anything that follows within 200ms.
func readPty(t *testing.T, m *os.File, want int) string {
	t.Helper()
	_ = m.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	var got []byte
	for {
		if len(got) >= want {
			_ = m.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		}
		n, err := m.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			return string(got)
		}
	}
}

// queuedInput is how many bytes wait in the terminal's input queue (in
// canonical mode, complete lines only).
func queuedInput(t *testing.T, s *os.File) int {
	t.Helper()
	n, err := unix.IoctlGetInt(int(s.Fd()), unix.TIOCINQ)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// On a terminal, however the attach ends, lever puts back the line settings
// it saw before (the fake attach leaves them raw, as a dying tmux does) and
// writes the mode resets exactly once. Input queued for the attach is dropped
// only after an abnormal end; after a detach it is the operator's typeahead.
func TestAttachRestoresTheTerminal(t *testing.T) {
	for _, tc := range fakeAttachEnds {
		t.Run(tc.name, func(t *testing.T) {
			m, s := openPty(t)
			tio, err := unix.IoctlGetTermios(int(s.Fd()), ioctlGetTermios)
			if err != nil {
				t.Fatal(err)
			}
			// No echo or output processing, so the master reads only lever's
			// bytes, as written.
			tio.Lflag |= unix.ICANON | unix.ISIG
			tio.Lflag &^= unix.ECHO
			tio.Oflag &^= unix.OPOST
			if err := unix.IoctlSetTermios(int(s.Fd()), unix.TCSETS, tio); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Write([]byte("\x1b[<0;1;1M\n")); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(2 * time.Second); queuedInput(t, s) == 0; {
				if time.Now().After(deadline) {
					t.Fatal("input never reached the terminal")
				}
				time.Sleep(10 * time.Millisecond)
			}
			_ = attachChild([]string{"sh", "-c", "stty raw -echo; " + tc.script}, s, s, s)
			after, err := unix.IoctlGetTermios(int(s.Fd()), ioctlGetTermios)
			if err != nil {
				t.Fatal(err)
			}
			if after.Lflag != tio.Lflag || after.Iflag != tio.Iflag || after.Oflag != tio.Oflag {
				t.Errorf("termios not restored: lflag %#x iflag %#x oflag %#x, want %#x %#x %#x",
					after.Lflag, after.Iflag, after.Oflag, tio.Lflag, tio.Iflag, tio.Oflag)
			}
			if q := queuedInput(t, s); (q == 0) != tc.abnormal {
				t.Errorf("%d bytes of input queued after the attach, want dropped only after an abnormal end", q)
			}
			want := attachResetSeq(tc.abnormal)
			if got := readPty(t, m, len(want)); got != want {
				t.Errorf("terminal got %q, want %q once", got, want)
			}
		})
	}
}

// The attach's argv[0] is the name it was given, not the resolved path.
func TestAttachChildKeepsArgv0(t *testing.T) {
	out := t.TempDir() + "/cmdline"
	if err := attachChild([]string{"sh", "-c", `cat /proc/$$/cmdline > "$0"`, out}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if !strings.HasPrefix(string(got), "sh\x00-c\x00") {
		t.Errorf("cmdline %q, want argv[0] sh", got)
	}
}
