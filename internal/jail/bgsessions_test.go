package jail

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

const testDomain = "linux:abc:pid:[4026532001]"

func TestParseBackgroundRecords(t *testing.T) {
	out := testDomain + "\n" +
		"@@file\n" + `{"pid":10,"kind":"interactive","sessionId":"s1","pidDomain":"` + testDomain + `"}` + "\n" + // live, ours
		"@@file\n" + `{"pid":11,"kind":"bg","sessionId":"s2","pidDomain":"linux:abc:pid:[4026532999]"}` + "\n" + // stale bg
		"@@file\n" + `{"pid":12,"kind":"interactive","parkedJobId":"j1","pidDomain":"linux:old:pid:[1]"}` + "\n" + // stale parked
		"@@file\n" + `{"pid":13,"kind":"bg","pidDomain":"` + testDomain + `"}` + "\n" + // bg in this container: not stale
		"@@file\n" + `{"pid":14,"kind":"bg"}` + "\n" + // no domain: cannot tell, stale
		"@@file\n" + `{"pid":15,"kind":"interactive","parkedJobId":null,"pidDomain":"x"}` + "\n" + // null parked: not background
		"@@file\n" + `{"pid":16,"kind":"bg", SECRET` + "\n" + // cut or broken
		"@@more\n"
	got, err := parseBackgroundRecords(out)
	if err != nil {
		t.Fatal(err)
	}
	want := BackgroundRecords{Stale: 3, Unreadable: 1, More: true}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got, err := parseBackgroundRecords(testDomain + "\n"); err != nil || got != (BackgroundRecords{}) {
		t.Fatalf("no records: %+v %v", got, err)
	}
	if _, err := parseBackgroundRecords("SECRET\n"); !errors.Is(err, ErrSessionsShape) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("no domain line: err = %v", err)
	}
}

func TestAgentProbeBackgroundSessions(t *testing.T) {
	var calls []string
	out := testDomain + "\n@@file\n" + `{"kind":"bg","pidDomain":"other"}` + "\n"
	got, err := AgentProbe{R: codeRunner{res: proc.Result{Stdout: out}, calls: &calls}}.BackgroundSessions(context.Background(), "lever--assistant")
	if err != nil || got.Stale != 1 {
		t.Fatalf("got %+v %v, want one stale record", got, err)
	}
	if !strings.Contains(calls[0], "podman exec --user scion lever--assistant sh -c") || !strings.HasSuffix(calls[0], "sh "+AgentSessionsDir) {
		t.Fatalf("argv %q", calls[0])
	}
	_, err = AgentProbe{R: codeRunner{res: proc.Result{Code: 1, Stderr: "SECRET"}, calls: &calls}}.BackgroundSessions(context.Background(), "x")
	var exit *ProbeExitError
	if !errors.As(err, &exit) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v, want a ProbeExitError without the container's words", err)
	}
	if ProbeErrorClass(ErrSessionsShape) != "unexpected output" {
		t.Fatal("ErrSessionsShape needs a fixed class")
	}
	if _, err := (AgentProbe{R: codeRunner{calls: &calls}}).BackgroundSessions(context.Background(), "--all"); err == nil {
		t.Fatal("a flag-shaped ref must be refused")
	}
}

// The script itself, under a real sh: the domain line, the record lines with
// folded newlines, the bounds, and a link skipped.
func TestBackgroundSessionsScriptRealShell(t *testing.T) {
	dir := t.TempDir()
	run := func() string {
		t.Helper()
		out, err := exec.Command("sh", "-c", bgSessionsScript, "sh", dir).Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if out := run(); !strings.HasPrefix(out, "linux:") || strings.Count(out, "\n") != 1 {
		t.Fatalf("empty dir: %q, want the domain line alone", out)
	}
	if out, err := exec.Command("sh", "-c", bgSessionsScript, "sh", filepath.Join(dir, "missing")).Output(); err != nil || strings.Count(string(out), "\n") != 1 {
		t.Fatalf("missing dir: %q %v", out, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte("{\n\"kind\": \"bg\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2.json"), []byte(`{"kind":"bg","pad":"`+strings.Repeat("a", 4000)+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(target, []byte(`{"kind":"bg"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "3.json")); err != nil {
		t.Fatal(err)
	}
	got, err := parseBackgroundRecords(run())
	if err != nil {
		t.Fatal(err)
	}
	// 1.json parses (folded newlines) and has no domain: stale. 2.json is
	// cut at the bound: unreadable. The link is skipped.
	if got != (BackgroundRecords{Stale: 1, Unreadable: 1}) {
		t.Fatalf("got %+v", got)
	}
	for i := 10; i < 10+maxSessionRecords+2; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", i)), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := run()
	if !strings.Contains(out, "@@more") || strings.Count(out, "@@file") != maxSessionRecords {
		t.Fatalf("bound: %d records read, more=%v", strings.Count(out, "@@file"), strings.Contains(out, "@@more"))
	}
}
