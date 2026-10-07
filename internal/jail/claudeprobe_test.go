package jail

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/wire"
)

func managedFile(window, note string) string {
	s := `{"permissions":{"deny":["x"]},"env":{"OTHER":"SECRET"`
	if window != "" {
		s += `,"` + wire.AutoCompactWindowEnv + `":"` + window + `"`
	}
	s += `},"hooks":{"SessionStart":[{"matcher":"*","hooks":[{"type":"command","command":"sciontool hook"}]}`
	if note != "" {
		s += `,{"matcher":"compact","hooks":[{"type":"command","command":"` + wire.AfterCompactHookPrefix + base64.StdEncoding.EncodeToString([]byte(note)) + `"}]}`
	}
	return s + `]}}`
}

func TestAgentProbeClaudeSettings(t *testing.T) {
	var calls []string
	p := AgentProbe{R: codeRunner{res: proc.Result{Stdout: managedFile("400000", "Re-read NOTES.md")}, calls: &calls}}
	got, err := p.ClaudeSettings(context.Background(), "lever--assistant")
	if err != nil {
		t.Fatal(err)
	}
	want := &wire.Claude{AutoCompactWindow: 400000, AfterCompactNote: "Re-read NOTES.md"}
	if !got.Matches(want) {
		t.Fatalf("delivered %+v does not match %+v", got, want)
	}
	if got.Matches(&wire.Claude{AutoCompactWindow: 400000, AfterCompactNote: "other"}) || got.Matches(nil) {
		t.Fatal("a different note or no config must not match")
	}
	if !strings.Contains(calls[0], "podman exec --user scion lever--assistant sh -c") || !strings.HasSuffix(calls[0], "sh "+wire.ManagedSettingsPath) {
		t.Fatalf("argv %q", calls[0])
	}
}

func TestAgentProbeClaudeSettingsAbsentAndErrors(t *testing.T) {
	var calls []string
	got, err := AgentProbe{R: codeRunner{res: proc.Result{Code: claudeAbsentExit}, calls: &calls}}.ClaudeSettings(context.Background(), "x")
	if err != nil || !got.Matches(nil) {
		t.Fatalf("absent file: %+v %v, want zero and no error", got, err)
	}
	_, err = AgentProbe{R: codeRunner{res: proc.Result{Code: 1, Stderr: "SECRET"}, calls: &calls}}.ClaudeSettings(context.Background(), "x")
	var exit *ProbeExitError
	if !errors.As(err, &exit) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v, want a ProbeExitError without the container's words", err)
	}
	for _, out := range []string{"SECRET not json", `{"env":{"` + wire.AutoCompactWindowEnv + `":"SECRET"}}`,
		`{"hooks":{"SessionStart":[{"hooks":[{"command":"` + wire.AfterCompactHookPrefix + `!!SECRET"}]}]}}`} {
		_, err := AgentProbe{R: codeRunner{res: proc.Result{Stdout: out}, calls: &calls}}.ClaudeSettings(context.Background(), "x")
		if !errors.Is(err, ErrClaudeShape) || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("output %q: err = %v, want ErrClaudeShape without echo", out, err)
		}
	}
	if ProbeErrorClass(ErrClaudeShape) != "unexpected output" {
		t.Fatal("ErrClaudeShape needs a fixed class")
	}
	if _, err := (AgentProbe{R: codeRunner{calls: &calls}}).ClaudeSettings(context.Background(), "--all"); err == nil {
		t.Fatal("a flag-shaped ref must be refused")
	}
}

// The script itself, under a real sh: the absent exit code, and the bound.
func TestClaudeSettingsScriptRealShell(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.json")
	err := exec.Command("sh", "-c", claudeSettingsScript, "sh", missing).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != claudeAbsentExit {
		t.Fatalf("missing file: %v, want exit %d", err, claudeAbsentExit)
	}
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", 40000)), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", claudeSettingsScript, "sh", big).Output()
	if err != nil || len(out) != 32768 {
		t.Fatalf("big file: %d bytes, %v; want 32768", len(out), err)
	}
}

func TestDescribeClaude(t *testing.T) {
	if got := DescribeClaude(nil); got != "none" {
		t.Errorf("nil = %q", got)
	}
	c := &wire.Claude{AutoCompactWindow: 400000, AfterCompactNote: "SECRET note"}
	if got := DescribeClaude(c); got != "window 400000, after-compact note" {
		t.Errorf("describe = %q", got)
	}
	d := ClaudeDelivered{AutoCompactWindow: 400000, NoteSum: NoteSum("x")}
	if got := d.Describe(); got != "window 400000, after-compact note" {
		t.Errorf("delivered describe = %q", got)
	}
}
