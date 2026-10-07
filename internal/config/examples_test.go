package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	for _, name := range []string{"hello-worker", "two-agents-comms", "multi-project"} {
		p := filepath.Join(root, "examples", name, "lever.yaml")
		app, err := Load(p)
		if err != nil {
			t.Fatalf("example %s: Load failed: %v", name, err)
		}
		if app.Name != name {
			t.Errorf("example %s: name=%q", name, app.Name)
		}
		if len(app.Workers) == 0 {
			t.Errorf("example %s: no workers", name)
		}
	}
}

// Every shipped example passes config load, the in-tree host path check
// included (the github tool's -tree, the todo tool's agent-editable -csv).
// Each is copied to a temp instance root with its tree created, and the
// /home/YOU placeholders point there. nested_virt is dropped off Linux,
// where it is not valid.
func TestEveryExampleLoads(t *testing.T) {
	examples, err := filepath.Glob(filepath.Join(repoRoot(t), "examples", "*", "lever.yaml"))
	if err != nil || len(examples) == 0 {
		t.Fatalf("no examples found: %v", err)
	}
	for _, src := range examples {
		name := filepath.Base(filepath.Dir(src))
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			body := strings.ReplaceAll(string(b), "/home/YOU/"+name, root)
			body = strings.ReplaceAll(body, "/home/YOU", filepath.Join(root, "home"))
			if runtime.GOOS != "linux" {
				body = strings.ReplaceAll(body, "nested_virt: true", "")
			}
			p := filepath.Join(root, CanonicalName)
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			probe, err := LoadNoHostChecks(p)
			if err != nil {
				t.Fatalf("example %s: %v", name, err)
			}
			// Again with the tree on disk, as after `lever init`.
			if err := os.MkdirAll(probe.Tree, 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadNoHostChecks(p); err != nil {
				t.Fatalf("example %s with its tree created: %v", name, err)
			}
		})
	}
}
