package jail

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/agent"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/wire"
)

// readyFixture is a container for readyScript: a fake /proc, a fake tmux on
// PATH that lists the panes in $FAKE_PANES (or fails when it is "fail"),
// and managed settings that name the hook.
type readyFixture struct {
	dir, bin, proc, marker, settings string
	panes                            string
}

func newReadyFixture(t *testing.T) *readyFixture {
	t.Helper()
	dir := t.TempDir()
	f := &readyFixture{
		dir: dir, bin: filepath.Join(dir, "bin"), proc: filepath.Join(dir, "proc"),
		marker: filepath.Join(dir, "lever-claude-ready"), settings: filepath.Join(dir, "managed-settings.json"),
		panes: "0 %0",
	}
	for _, d := range []string{f.bin, filepath.Join(f.proc, "sys/kernel/random"), filepath.Join(f.proc, "1")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tmux := "#!/bin/sh\n[ \"$FAKE_PANES\" = fail ] && { echo \"can't find window: agent\" >&2; exit 1; }\nprintf '%s\\n' \"$FAKE_PANES\"\n"
	write(t, filepath.Join(f.bin, "tmux"), tmux, 0o755)
	write(t, filepath.Join(f.proc, "sys/kernel/random/boot_id"), "3f1e2d3c-aaaa-bbbb-cccc-0123456789ab\n", 0o644)
	f.setStart(t, "4242")
	write(t, f.settings, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"`+wire.ReadyHookCommand+`"}]}]}}`, 0o644)
	return f
}

func write(t *testing.T, path, s string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), mode); err != nil {
		t.Fatal(err)
	}
}

// setStart writes PID 1's stat with start time start; its command name has
// a space and a parenthesis, as a parse that splits on the first ')' would
// get wrong.
func (f *readyFixture) setStart(t *testing.T, start string) {
	t.Helper()
	write(t, filepath.Join(f.proc, "1/stat"), "1 (init (x) y) S 0 1 1 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 "+start+" 1000 200\n", 0o644)
}

// writeMarker writes the marker as the hook does, for pane.
func (f *readyFixture) writeMarker(t *testing.T, pane string) {
	t.Helper()
	if err := agent.WriteReadyMarker(f.marker, f.proc, pane); err != nil {
		t.Fatal(err)
	}
}

func (f *readyFixture) run(t *testing.T, polls int) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", readyScript, "sh", f.marker, strconv.Itoa(polls), wire.ReadyHookCommand, f.settings, f.proc)
	cmd.Env = append(os.Environ(), "PATH="+f.bin+":"+os.Getenv("PATH"), "FAKE_PANES="+f.panes)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// The script and agent.WriteReadyMarker agree on the marker: the hook's
// marker for the live pane reads ready.
func TestReadyScriptMatchesTheHooksMarker(t *testing.T) {
	f := newReadyFixture(t)
	f.writeMarker(t, "%0")
	if got := f.run(t, 1); got != "ready" {
		t.Fatalf("got %q, want ready", got)
	}
}

func TestReadyScriptPendingWithoutMarker(t *testing.T) {
	f := newReadyFixture(t)
	start := time.Now()
	if got := f.run(t, 2); got != "pending" {
		t.Fatalf("got %q, want pending", got)
	}
	if el := time.Since(start); el < readyStep {
		t.Fatalf("two polls took %s: the script did not wait between them", el)
	}
}

// A marker from a previous container run (another PID 1 start time) never
// counts.
func TestReadyScriptStaleMarkerIsPending(t *testing.T) {
	f := newReadyFixture(t)
	f.setStart(t, "1000")
	f.writeMarker(t, "%0")
	f.setStart(t, "2000")
	if got := f.run(t, 1); got != "pending" {
		t.Fatalf("got %q, want pending for a stale marker", got)
	}
}

// A marker for another pane (an agent's own claude in a pane of its own)
// does not count; one for a live pane of the agent window does.
func TestReadyScriptMarkerMustNameTheAgentPane(t *testing.T) {
	f := newReadyFixture(t)
	f.writeMarker(t, "%7")
	if got := f.run(t, 1); got != "pending" {
		t.Fatalf("got %q, want pending for another pane's marker", got)
	}
	f.panes = "1 %0\n0 %7"
	if got := f.run(t, 1); got != "ready" {
		t.Fatalf("got %q, want ready", got)
	}
}

func TestReadyScriptSymlinkMarkerIsPending(t *testing.T) {
	f := newReadyFixture(t)
	real := filepath.Join(f.dir, "elsewhere")
	if err := agent.WriteReadyMarker(real, f.proc, "%0"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, f.marker); err != nil {
		t.Fatal(err)
	}
	if got := f.run(t, 1); got != "pending" {
		t.Fatalf("got %q, want pending for a symbolic link", got)
	}
}

// An oversized marker is read only up to its bound and does not match.
func TestReadyScriptOversizedMarkerIsPending(t *testing.T) {
	f := newReadyFixture(t)
	write(t, f.marker, strings.Repeat("v1 ", 100000), 0o600)
	if got := f.run(t, 1); got != "pending" {
		t.Fatalf("got %q, want pending", got)
	}
}

func TestReadyScriptNoHook(t *testing.T) {
	f := newReadyFixture(t)
	write(t, f.settings, `{"env":{}}`, 0o644)
	f.writeMarker(t, "%0")
	if got := f.run(t, 1); got != "nohook" {
		t.Fatalf("got %q, want nohook", got)
	}
	os.Remove(f.settings)
	if got := f.run(t, 1); got != "nohook" {
		t.Fatalf("got %q, want nohook for a missing file", got)
	}
}

func TestReadyScriptPaneGone(t *testing.T) {
	f := newReadyFixture(t)
	f.writeMarker(t, "%0")
	for _, panes := range []string{"1 %0", "fail"} {
		f.panes = panes
		if got := f.run(t, 1); got != "gone" {
			t.Fatalf("panes %q: got %q, want gone", panes, got)
		}
	}
}

// The script's bound on the marker read is the wire bound.
func TestReadyScriptBoundIsTheWireBound(t *testing.T) {
	if !strings.Contains(readyScript, "head -c "+strconv.Itoa(wire.ReadyMarkerMax)+" ") {
		t.Fatalf("readyScript does not read the marker with the %d-byte bound", wire.ReadyMarkerMax)
	}
	if !strings.Contains(readyScript, `"v1 $boot $st $id"`) || wire.ReadyMarker("b", "s", "p") != "v1 b s p" {
		t.Fatal("readyScript and wire.ReadyMarker disagree on the marker's form")
	}
}

func TestAgentProbeAwaitClaudeReady(t *testing.T) {
	for _, c := range []struct {
		out  string
		want ClaudeReady
	}{
		{"ready\n", ClaudeReadyYes},
		{"pending\n", ClaudeReadyPending},
		{"nohook\n", ClaudeReadyNoHook},
		{"gone\n", ClaudeReadyPaneGone},
	} {
		host := proc.NewFakeRunner()
		host.Script("orb", proc.Result{Stdout: c.out})
		jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
		got, err := AgentProbe{R: jr}.AwaitClaudeReady(context.Background(), "lever--assistant", 4*time.Second)
		if err != nil || got != c.want {
			t.Fatalf("%q: got %v, %v; want %v", c.out, got, err, c.want)
		}
		argv := host.Calls[0].Argv()
		if !strings.Contains(argv, "podman exec --user scion lever--assistant sh -c") ||
			!strings.HasSuffix(argv, "sh "+wire.ReadyMarkerPath+" 20 "+wire.ReadyHookCommand+" "+wire.ManagedSettingsPath+" /proc") {
			t.Fatalf("argv %q", argv)
		}
	}
}

// Output that is not one of the four words is an error that repeats none
// of it.
func TestAgentProbeAwaitClaudeReadyOddOutput(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{Stdout: "ready SECRET\n"})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	_, err := AgentProbe{R: jr}.AwaitClaudeReady(context.Background(), "lever--assistant", time.Second)
	if !errors.Is(err, ErrReadyShape) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("got %v", err)
	}
	if ProbeErrorClass(err) != "unexpected output" {
		t.Fatalf("class %q", ProbeErrorClass(err))
	}
}

func TestAgentProbeAwaitClaudeReadyNoContainer(t *testing.T) {
	jr := New(Config{Host: failingRunner{`Error: no such container "x"`}, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	if _, err := (AgentProbe{R: jr}).AwaitClaudeReady(context.Background(), "x", time.Second); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("want ErrNoContainer, got %v", err)
	}
	host := proc.NewFakeRunner()
	if _, err := (AgentProbe{R: host}).AwaitClaudeReady(context.Background(), "--all", time.Second); err == nil || len(host.Calls) != 0 {
		t.Fatal("a flag-shaped ref must be refused before any call")
	}
}
