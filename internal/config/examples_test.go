package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("cannot locate test file")
	}
	// this file: <repo>/internal/config/examples_test.go → repo root is two dirs up
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

func TestShippedExamplesLoadAndValidate(t *testing.T) {
	root := repoRoot(t)
	for _, name := range []string{"hello-grove", "two-agents-comms", "multi-project"} {
		p := filepath.Join(root, "examples", name, "lever.yaml")
		app, err := Load(p)
		if err != nil {
			t.Fatalf("example %s: Load failed: %v", name, err)
		}
		if app.Name != name {
			t.Errorf("example %s: name=%q", name, app.Name)
		}
		if len(app.Groves) == 0 {
			t.Errorf("example %s: no groves", name)
		}
	}
}
