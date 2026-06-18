package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lever-to/lever/internal/config"
)

// instanceDir creates a temp dir containing a canonical lever.yaml for the given
// app name and returns the dir.
func instanceDir(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	body := "name: " + name + "\nbackend: orbstack\nmanager:\n  image: img:1\n"
	if err := os.WriteFile(filepath.Join(dir, config.CanonicalName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveConfigPathExplicitWins(t *testing.T) {
	got, err := resolveConfigPath("/some/explicit.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/some/explicit.yaml" {
		t.Fatalf("explicit arg should pass through, got %q", got)
	}
}

func TestResolveConfigPathDiscoversFromCwd(t *testing.T) {
	dir := instanceDir(t, "demo")
	sub := filepath.Join(dir, "nested", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	got, err := resolveConfigPath("")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	want := filepath.Join(dir, config.CanonicalName)
	// macOS temp dirs are symlinked (/var → /private/var); compare resolved.
	gotR, _ := filepath.EvalSymlinks(got)
	wantR, _ := filepath.EvalSymlinks(want)
	if gotR != wantR {
		t.Fatalf("discovered %q want %q", gotR, wantR)
	}
}

func TestMachineFromFlagOrConfig(t *testing.T) {
	// explicit flag wins, no config needed
	if m, err := machineFromFlagOrConfig("lever-custom"); err != nil || m != "lever-custom" {
		t.Fatalf("explicit machine: got %q err %v", m, err)
	}
	// no flag → derived from discovered config
	dir := instanceDir(t, "assistant")
	t.Chdir(dir)
	m, err := machineFromFlagOrConfig("")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if m != "lever-assistant" {
		t.Fatalf("derived machine = %q, want lever-assistant", m)
	}
}
