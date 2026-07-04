package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/config"
)

func TestClearStagedRuntimeState(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(filepath.Join(tree, ".lever"), 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(tree, ".lever", "bootstrap.json")
	manifest := filepath.Join(tree, config.ManifestName)
	if err := os.WriteFile(bootstrap, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	clearStagedRuntimeState(&config.App{Tree: tree})

	if _, err := os.Stat(bootstrap); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bootstrap.json should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(manifest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest should be removed, stat err = %v", err)
	}
	// The now-empty .lever dir should be gone too.
	if _, err := os.Stat(filepath.Join(tree, ".lever")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty .lever dir should be removed, stat err = %v", err)
	}
}

func TestClearStagedRuntimeStateMissingIsNoop(t *testing.T) {
	// Nothing staged: must not panic or error.
	clearStagedRuntimeState(&config.App{Tree: t.TempDir()})
}

// TestDestroyCallsTeardown verifies the renamed command (Use: "destroy")
// still tears the jail down.
func TestDestroyCallsTeardown(t *testing.T) {
	sb := &stubBackend{}
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return sb, nil })
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"destroy", "--machine", "lever-x"})
	if err := root.Execute(); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if !sb.down {
		t.Fatal("destroy must call Backend.Teardown")
	}
}

// TestDownAliasCallsTeardown is the deprecation-safety regression: `lever
// down` must keep working, unchanged, as a hidden alias of `destroy`.
func TestDownAliasCallsTeardown(t *testing.T) {
	sb := &stubBackend{}
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return sb, nil })
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"down", "--machine", "lever-x"})
	if err := root.Execute(); err != nil {
		t.Fatalf("down (alias): %v", err)
	}
	if !sb.down {
		t.Fatal("the `down` alias must still call Backend.Teardown")
	}
}
