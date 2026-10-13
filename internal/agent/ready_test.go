package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/wire"
)

// fakeProc writes a /proc with a boot id and a PID 1 stat whose command
// name holds a space and a parenthesis.
func fakeProc(t *testing.T, boot, start string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sys/kernel/random"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sys/kernel/random/boot_id"), []byte(boot+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Fields 3..21, then 22 (start time), then more.
	stat := "1 (scion tool) S 0 1 1 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 " + start + " 1000 200 18446744073709551615\n"
	if err := os.WriteFile(filepath.Join(root, "1/stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRunIdentityParsesField22(t *testing.T) {
	proc := fakeProc(t, "0f6c4b6e-6a43-4a6e-9a1b-3c2d1e0f9a8b", "123456")
	boot, start, err := RunIdentity(proc)
	if err != nil {
		t.Fatal(err)
	}
	if boot != "0f6c4b6e-6a43-4a6e-9a1b-3c2d1e0f9a8b" || start != "123456" {
		t.Fatalf("identity = %q %q", boot, start)
	}
}

func TestRunIdentityRefusesOddStat(t *testing.T) {
	proc := fakeProc(t, "b", "1")
	if err := os.WriteFile(filepath.Join(proc, "1/stat"), []byte("1 (x) S 0 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunIdentity(proc); err == nil {
		t.Fatal("a short stat parsed")
	}
	if err := os.WriteFile(filepath.Join(proc, "1/stat"), []byte("1 (x) S 0 1 1 0 -1 4 1 0 0 0 1 2 0 0 20 0 1 0 12a 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunIdentity(proc); err == nil {
		t.Fatal("a start time that is not a number parsed")
	}
}

func TestWriteReadyMarker(t *testing.T) {
	proc := fakeProc(t, "boot-1", "777")
	path := filepath.Join(t.TempDir(), "lever-claude-ready")
	if err := WriteReadyMarker(path, proc, "%3"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), wire.ReadyMarker("boot-1", "777", "%3")+"\n"; got != want {
		t.Fatalf("marker = %q, want %q", got, want)
	}
	if len(b) > wire.ReadyMarkerMax {
		t.Fatalf("marker is %d bytes, over the reader's bound", len(b))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	// No temporary file is left behind.
	ents, _ := os.ReadDir(filepath.Dir(path))
	if len(ents) != 1 {
		t.Fatalf("directory holds %d entries, want the marker alone", len(ents))
	}
}

// A claude outside a tmux pane (boot's claude mcp add, a claude -p through
// podman exec) is not the harness: no marker.
func TestWriteReadyMarkerOutsideAPaneWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lever-claude-ready")
	if err := WriteReadyMarker(path, fakeProc(t, "b", "1"), ""); !errors.Is(err, ErrNotHarness) {
		t.Fatalf("want ErrNotHarness, got %v", err)
	}
	if err := WriteReadyMarker(path, fakeProc(t, "b", "1"), "%1;rm"); err == nil {
		t.Fatal("an odd TMUX_PANE was accepted")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("a marker was written: %v", err)
	}
}

// A symbolic link planted at the marker path is replaced, never followed.
func TestWriteReadyMarkerReplacesASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "lever-claude-ready")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := WriteReadyMarker(path, fakeProc(t, "b", "1"), "%0"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("the link target was written: %q", b)
	}
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("marker is still a link (%v)", err)
	}
}

// Every start rewrites the managed settings: one claude-ready hook, never two.
func TestManagedSettingsCarryOneReadyHook(t *testing.T) {
	p := managedPath(t)
	for range 3 {
		if err := WriteManagedSettings(p, &wire.Claude{AfterCompactNote: "n"}); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), `"`+ReadyHookCommand+`"`); n != 1 {
		t.Fatalf("claude-ready hook appears %d times, want 1:\n%s", n, b)
	}
}
