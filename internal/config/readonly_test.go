package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stevegeek/lever/internal/testutil"
)

// readOnlyApp builds a minimal valid App with the given manager.read_only
// entries and one worker at workers/w.
func readOnlyApp(t *testing.T, entries ...string) *App {
	t.Helper()
	a := testApp(t, "workers/w", "workers/v")
	a.Manager.ReadOnly = entries
	return a
}

func TestValidateAcceptsManagerReadOnly(t *testing.T) {
	app := readOnlyApp(t, "assistant/tools", "bin", "assistant/lib/ruby")
	if err := app.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// Loaded from YAML too: the key is manager.read_only.
	body := "name: demo\nbackend: orbstack\ntree: ws\nmanager:\n  read_only:\n    - assistant/tools\n"
	loaded, err := LoadNoHostChecks(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := []string{"assistant/tools"}; !reflect.DeepEqual(loaded.Manager.ReadOnly, want) {
		t.Fatalf("read_only = %q, want %q", loaded.Manager.ReadOnly, want)
	}
}

// Every refusal names manager.read_only (and, for the worker case, the
// worker and its dir), so the operator can find the line.
func TestValidateRejectsBadManagerReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		want    []string
	}{
		{"empty", []string{""}, []string{"manager.read_only", "empty"}},
		{"absolute", []string{"/etc"}, []string{"manager.read_only", `"/etc"`}},
		{"dot", []string{"."}, []string{"manager.read_only", `"."`}},
		{"dot slash", []string{"./tools"}, []string{"manager.read_only", `"./tools"`}},
		{"trailing slash", []string{"tools/"}, []string{"manager.read_only", `"tools/"`}},
		{"double slash", []string{"a//tools"}, []string{"manager.read_only"}},
		{"parent", []string{".."}, []string{"manager.read_only"}},
		{"escape", []string{"../outside"}, []string{"manager.read_only", "../outside"}},
		{"inner dotdot", []string{"a/../b"}, []string{"manager.read_only"}},
		{"duplicate", []string{"tools", "tools"}, []string{"manager.read_only", "twice"}},
		{"nested, outer first", []string{"assistant", "assistant/tools"}, []string{"nested", `"assistant"`, `"assistant/tools"`}},
		{"nested, inner first", []string{"assistant/tools", "assistant"}, []string{"nested"}},
		{"worker equals entry", []string{"workers/w"}, []string{`worker "alpha"`, `"workers/w"`, "manager.read_only"}},
		{"worker inside entry", []string{"workers"}, []string{`worker "alpha"`, "manager.read_only"}},
		{"worker contains entry", []string{"workers/v/tools"}, []string{`worker "beta"`, `"workers/v"`, `"workers/v/tools"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testutil.WantErrContaining(t, readOnlyApp(t, tc.entries...).Validate(), tc.want...)
		})
	}
	// A sibling that merely shares a prefix is not an overlap.
	if err := readOnlyApp(t, "workers/wx").Validate(); err != nil {
		t.Fatalf("workers/wx beside worker dir workers/w must be accepted: %v", err)
	}
}

func TestManagerTreeMounts(t *testing.T) {
	if got := readOnlyApp(t).ManagerTreeMounts(); got != nil {
		t.Fatalf("no read_only must plan no mounts, got %v", got)
	}
	app := readOnlyApp(t, "assistant/tools", "bin", "assistant/lib/ruby", "assistant/lib/go")
	want := []TreeMount{
		// Depth 0: a top-level entry has no pin (its parent is the workspace mount).
		{Rel: "assistant", ReadOnly: false},
		{Rel: "bin", ReadOnly: true},
		// Depth 1: the shared pin assistant/lib appears once.
		{Rel: "assistant/lib", ReadOnly: false},
		{Rel: "assistant/tools", ReadOnly: true},
		{Rel: "assistant/lib/go", ReadOnly: true},
		{Rel: "assistant/lib/ruby", ReadOnly: true},
	}
	if got := app.ManagerTreeMounts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ManagerTreeMounts =\n%v\nwant\n%v", got, want)
	}
}

func TestCheckManagerReadOnlyHost(t *testing.T) {
	mk := func(t *testing.T) *App {
		t.Helper()
		app := readOnlyApp(t, "assistant/tools")
		if err := os.MkdirAll(filepath.Join(app.Tree, "assistant", "tools"), 0o755); err != nil {
			t.Fatal(err)
		}
		return app
	}
	t.Run("real directories", func(t *testing.T) {
		if err := mk(t).CheckManagerReadOnlyHost(); err != nil {
			t.Fatalf("CheckManagerReadOnlyHost: %v", err)
		}
	})
	t.Run("nothing configured", func(t *testing.T) {
		if err := readOnlyApp(t).CheckManagerReadOnlyHost(); err != nil {
			t.Fatalf("CheckManagerReadOnlyHost: %v", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		app := readOnlyApp(t, "assistant/tools")
		testutil.WantErrContaining(t, app.CheckManagerReadOnlyHost(), "manager.read_only", "assistant/tools")
	})
	t.Run("a file, not a directory", func(t *testing.T) {
		app := readOnlyApp(t, "assistant/tools")
		if err := os.MkdirAll(filepath.Join(app.Tree, "assistant"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(app.Tree, "assistant", "tools"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		testutil.WantErrContaining(t, app.CheckManagerReadOnlyHost(), "not a directory")
	})
	t.Run("the entry is a symlink", func(t *testing.T) {
		app := mk(t)
		elsewhere := t.TempDir()
		tools := filepath.Join(app.Tree, "assistant", "tools")
		if err := os.Remove(tools); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, tools); err != nil {
			t.Fatal(err)
		}
		testutil.WantErrContaining(t, app.CheckManagerReadOnlyHost(), "symbolic link", tools)
	})
	t.Run("an ancestor is a symlink", func(t *testing.T) {
		app := readOnlyApp(t, "assistant/tools")
		real := filepath.Join(t.TempDir(), "real")
		if err := os.MkdirAll(filepath.Join(real, "tools"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(app.Tree, "assistant")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		testutil.WantErrContaining(t, app.CheckManagerReadOnlyHost(), "symbolic link", link)
	})
}
