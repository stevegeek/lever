package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/wire"
)

// ReadyHookCommand and ReadyMarkerPath: see wire.
const (
	ReadyHookCommand = wire.ReadyHookCommand
	ReadyMarkerPath  = wire.ReadyMarkerPath
)

// ErrNotHarness is WriteReadyMarker's answer for a session outside a tmux
// pane: a claude that is not scion's harness (boot's `claude mcp add`, a
// `claude -p` through podman exec). It writes nothing.
var ErrNotHarness = errors.New("not in a tmux pane")

// WriteReadyMarker is the claude-ready hook (lever#188): it writes
// wire.ReadyMarker for this container run and pane to path. procRoot is the
// /proc to read the run's identity from (a test seam); pane is $TMUX_PANE.
//
// The marker only claims that a claude session started in that pane. The
// agent can write it itself, which claims no more than a live pane already
// does, and the manager gate compares it for equality only.
//
// The file is written next to path (0600: the probe reads it as the same
// user) and renamed over it, so a symbolic link planted at path is
// replaced, never followed.
func WriteReadyMarker(path, procRoot, pane string) error {
	if pane == "" {
		return ErrNotHarness
	}
	if !validPaneID(pane) {
		return fmt.Errorf("claude-ready: unexpected TMUX_PANE %q", pane)
	}
	boot, start, err := RunIdentity(procRoot)
	if err != nil {
		return fmt.Errorf("claude-ready: %w", err)
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".lever-claude-ready-*")
	if err != nil {
		return fmt.Errorf("claude-ready: %w", err)
	}
	tmp := f.Name()
	_, werr := f.WriteString(wire.ReadyMarker(boot, start, pane) + "\n")
	if err := errors.Join(werr, f.Close()); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("claude-ready: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("claude-ready: %w", err)
	}
	return nil
}

// validPaneID reports whether s is a tmux pane id: "%" and digits.
func validPaneID(s string) bool {
	rest, ok := strings.CutPrefix(s, "%")
	if !ok || rest == "" || len(rest) > 10 {
		return false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// RunIdentity names this container run: the guest kernel's boot id and the
// start time of the container's PID 1 (field 22 of <procRoot>/1/stat). A
// new container, or the same one started again, has a new PID 1; a guest
// reboot has a new boot id. The manager gate's reader computes the same two
// values in shell (jail's readyScript) and must agree with this parse.
func RunIdentity(procRoot string) (bootID, pid1Start string, err error) {
	b, err := readBounded(filepath.Join(procRoot, "sys/kernel/random/boot_id"), 128)
	if err != nil {
		return "", "", fmt.Errorf("boot id: %w", err)
	}
	bootID = strings.TrimSpace(b)
	if bootID == "" || strings.ContainsAny(bootID, " \t\n") {
		return "", "", errors.New("boot id: unexpected content")
	}
	s, err := readBounded(filepath.Join(procRoot, "1/stat"), 1024)
	if err != nil {
		return "", "", fmt.Errorf("pid 1 stat: %w", err)
	}
	// The command name (field 2) is in parentheses and may hold spaces and
	// parentheses: the fields after it start after the last ')'.
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return "", "", errors.New("pid 1 stat: unexpected content")
	}
	fields := strings.Fields(s[i+1:])
	// fields[0] is field 3 (state), so field 22 is fields[19].
	if len(fields) < 20 {
		return "", "", errors.New("pid 1 stat: too few fields")
	}
	pid1Start = fields[19]
	for _, c := range pid1Start {
		if c < '0' || c > '9' {
			return "", "", errors.New("pid 1 stat: start time is not a number")
		}
	}
	return bootID, pid1Start, nil
}

func readBounded(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, n))
	return string(b), err
}
