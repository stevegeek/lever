package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend/registry"
	"github.com/stevegeek/lever/internal/config"
)

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
	dir := writeInstance(t, managerYAML)

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

func TestInstanceAppMachine(t *testing.T) {
	// explicit flag wins, no config needed
	if m, err := (instanceApp{err: os.ErrNotExist}).machine("lever-custom"); err != nil || m != "lever-custom" {
		t.Fatalf("explicit machine: got %q err %v", m, err)
	}
	// no flag, no config → the error says so
	if _, err := (instanceApp{err: os.ErrNotExist}).machine(""); err == nil || !strings.Contains(err.Error(), "no --machine given") {
		t.Fatalf("no config: err = %v", err)
	}
	// no flag → derived from discovered config
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)
	ia := loadInstanceApp()
	if ia.err != nil || ia.app == nil || ia.path == "" {
		t.Fatalf("loadInstanceApp = %+v", ia)
	}
	m, err := ia.machine("")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if m != "lever-demo" {
		t.Fatalf("derived machine = %q, want lever-demo", m)
	}
}

func TestInstanceAppBackend(t *testing.T) {
	none := instanceApp{err: os.ErrNotExist}
	if got := none.backend("lima"); got != "lima" {
		t.Fatalf("flag must win, got %q", got)
	}
	if got := none.backend(""); got != registry.Default {
		t.Fatalf("no config must fall back to the registry default, got %q", got)
	}
	if got := (instanceApp{app: &config.App{Backend: "orbstack"}}).backend(""); got != "orbstack" {
		t.Fatalf("config backend must be used, got %q", got)
	}
}

func TestLoadAppPath(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)
	path, app, err := loadAppPath(nil)
	if err != nil || app == nil || app.Name != "demo" || filepath.Base(path) != config.CanonicalName {
		t.Fatalf("loadAppPath(nil) = %q %+v %v", path, app, err)
	}
	if _, _, err := loadAppPath([]string{filepath.Join(dir, "missing.yaml")}); err == nil {
		t.Fatal("an explicit missing config must fail")
	}
}
