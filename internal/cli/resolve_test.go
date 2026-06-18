package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lever-to/lever/internal/config"
)

// instanceDir creates a temp dir containing a canonical lever.yaml for the given
// app name (with a confined tree subdir) and returns the dir.
func instanceDir(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	body := "name: " + name + "\nbackend: orbstack\ntree: workspace\nmanager:\n  image: img:1\n"
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

func TestResolveConfigPathCwdOnlyNoWalkUp(t *testing.T) {
	dir := instanceDir(t, "demo")

	// In the instance root → found.
	t.Chdir(dir)
	got, err := resolveConfigPath("")
	if err != nil {
		t.Fatalf("cwd resolve: %v", err)
	}
	gotR, _ := filepath.EvalSymlinks(got)
	wantR, _ := filepath.EvalSymlinks(filepath.Join(dir, config.CanonicalName))
	if gotR != wantR {
		t.Fatalf("resolved %q want %q", gotR, wantR)
	}

	// In a SUBDIR → must NOT walk up; no config in cwd → error (security: a
	// planted parent config must never be picked up).
	sub := filepath.Join(dir, "nested", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	if _, err := resolveConfigPath(""); err == nil {
		t.Fatal("resolveConfigPath must not walk up to a parent config")
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
