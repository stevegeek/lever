package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stevegeek/lever/internal/testutil"
)

// readOnlyApp builds a minimal valid App with the given manager.read_only
// entries and two workers, alpha at workers/w and beta at workers/v.
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
		{"dollar", []string{"$HOME/tools"}, []string{"manager.read_only", "$VAR"}},
		{"tilde", []string{"~/tools"}, []string{"manager.read_only", "~"}},
		{"colon", []string{"a:ro"}, []string{"manager.read_only"}},
		{"duplicate", []string{"tools", "tools"}, []string{"manager.read_only", "twice"}},
		{"duplicate by case", []string{"tools", "Tools"}, []string{"manager.read_only", "twice"}},
		{"nested, outer first", []string{"assistant", "assistant/tools"}, []string{"nested", `"assistant"`, `"assistant/tools"`}},
		{"nested, inner first", []string{"assistant/tools", "assistant"}, []string{"nested"}},
		{"nested by case", []string{"Assistant", "assistant/tools"}, []string{"nested"}},
		{"worker equals entry", []string{"workers/w"}, []string{`worker "alpha"`, `"workers/w"`, "manager.read_only"}},
		{"worker equals entry by case", []string{"Workers/W"}, []string{`worker "alpha"`, "manager.read_only"}},
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
	// A worker dir becomes a pin while read_only is set: same character rule.
	app := readOnlyApp(t, "tools")
	app.Workers[0].Dir = "workers/$X"
	testutil.WantErrContaining(t, app.Validate(), `worker "alpha"`, "workers/$X")
	// ...but only then: without read_only nothing about worker dirs changes.
	app.Manager.ReadOnly = nil
	if err := app.Validate(); err != nil {
		t.Fatalf("without read_only a worker dir keeps today's rules: %v", err)
	}
}

func TestManagerTreeMounts(t *testing.T) {
	if got := readOnlyApp(t).ManagerTreeMounts(); got != nil {
		t.Fatalf("no read_only must plan no mounts (not even worker pins), got %v", got)
	}
	app := readOnlyApp(t, "assistant/tools", "bin", "assistant/lib/ruby", "assistant/lib/go")
	app.Workers = append(app.Workers, Worker{Name: "gamma", Dir: "./Assistant/notes/"})
	want := []TreeMount{
		// Depth 0: a top-level entry has no pin (its parent is the workspace mount).
		{Rel: "assistant", ReadOnly: false},
		{Rel: "bin", ReadOnly: true},
		{Rel: "workers", ReadOnly: false},
		// Depth 1: the shared pin assistant/lib appears once; each worker
		// dir is pinned; gamma's ancestor "Assistant" folds into "assistant".
		{Rel: "Assistant/notes", ReadOnly: false},
		{Rel: "assistant/lib", ReadOnly: false},
		{Rel: "assistant/tools", ReadOnly: true},
		{Rel: "workers/v", ReadOnly: false},
		{Rel: "workers/w", ReadOnly: false},
		{Rel: "assistant/lib/go", ReadOnly: true},
		{Rel: "assistant/lib/ruby", ReadOnly: true},
	}
	if got := app.ManagerTreeMounts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ManagerTreeMounts =\n%v\nwant\n%v", got, want)
	}
}

func TestPrepareManagerReadOnlyHost(t *testing.T) {
	mk := func(t *testing.T) *App {
		t.Helper()
		app := readOnlyApp(t, "assistant/tools")
		if err := os.MkdirAll(filepath.Join(app.Tree, "assistant", "tools"), 0o755); err != nil {
			t.Fatal(err)
		}
		return app
	}
	t.Run("real directories; missing worker dirs are created", func(t *testing.T) {
		app := mk(t)
		if err := app.PrepareManagerReadOnlyHost(); err != nil {
			t.Fatalf("PrepareManagerReadOnlyHost: %v", err)
		}
		for _, d := range []string{"workers/w", "workers/v"} {
			if fi, err := os.Lstat(filepath.Join(app.Tree, d)); err != nil || !fi.IsDir() {
				t.Fatalf("worker dir %s must be created for its pin: %v", d, err)
			}
		}
	})
	t.Run("nothing configured touches nothing", func(t *testing.T) {
		app := readOnlyApp(t)
		if err := app.PrepareManagerReadOnlyHost(); err != nil {
			t.Fatalf("PrepareManagerReadOnlyHost: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(app.Tree, "workers")); err == nil {
			t.Fatal("without read_only no worker dir may be created")
		}
	})
	t.Run("missing", func(t *testing.T) {
		app := readOnlyApp(t, "assistant/tools")
		testutil.WantErrContaining(t, app.PrepareManagerReadOnlyHost(), "manager.read_only", "assistant/tools")
	})
	t.Run("a file, not a directory", func(t *testing.T) {
		app := readOnlyApp(t, "assistant/tools")
		if err := os.MkdirAll(filepath.Join(app.Tree, "assistant"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(app.Tree, "assistant", "tools"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		testutil.WantErrContaining(t, app.PrepareManagerReadOnlyHost(), "not a directory")
	})
	t.Run("the entry is a symlink", func(t *testing.T) {
		app := mk(t)
		tools := filepath.Join(app.Tree, "assistant", "tools")
		if err := os.Remove(tools); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), tools); err != nil {
			t.Fatal(err)
		}
		testutil.WantErrContaining(t, app.PrepareManagerReadOnlyHost(), "symbolic link", tools)
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
		testutil.WantErrContaining(t, app.PrepareManagerReadOnlyHost(), "symbolic link", link)
	})
	// The swap the worker-dir pins exist to stop: the manager replaces a
	// worker dir (or its parent) with a link to the protected entry.
	t.Run("a worker dir is a link to the entry", func(t *testing.T) {
		app := mk(t)
		if err := os.MkdirAll(filepath.Join(app.Tree, "workers"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(app.Tree, "workers", "w")
		if err := os.Symlink("../assistant/tools", link); err != nil {
			t.Fatal(err)
		}
		testutil.WantErrContaining(t, app.PrepareManagerReadOnlyHost(), `worker "alpha"`, "symbolic link", link)
	})
	t.Run("the workers ancestor is a link", func(t *testing.T) {
		app := mk(t)
		link := filepath.Join(app.Tree, "workers")
		if err := os.Symlink("assistant", link); err != nil {
			t.Fatal(err)
		}
		testutil.WantErrContaining(t, app.PrepareManagerReadOnlyHost(), "worker", "symbolic link", link)
		if _, err := os.Lstat(filepath.Join(app.Tree, "assistant", "w")); err == nil {
			t.Fatal("SECURITY: a worker dir was created through the link")
		}
	})
	t.Run("a link inside the entry pointing out", func(t *testing.T) {
		app := mk(t)
		link := filepath.Join(app.Tree, "assistant", "tools", "lib")
		if err := os.Symlink("../notes", link); err != nil {
			t.Fatal(err)
		}
		testutil.WantErrContaining(t, app.PrepareManagerReadOnlyHost(), "symbolic link", link, "outside the entry")
	})
	t.Run("an absolute link inside the entry pointing out", func(t *testing.T) {
		app := mk(t)
		link := filepath.Join(app.Tree, "assistant", "tools", "deep", "gem")
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), link); err != nil {
			t.Fatal(err)
		}
		testutil.WantErrContaining(t, app.PrepareManagerReadOnlyHost(), link, "outside the entry")
	})
	t.Run("a link inside the entry pointing inside is fine", func(t *testing.T) {
		app := mk(t)
		tools := filepath.Join(app.Tree, "assistant", "tools")
		if err := os.MkdirAll(filepath.Join(tools, "lib", "v2"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("lib/v2", filepath.Join(tools, "current")); err != nil {
			t.Fatal(err)
		}
		if err := app.PrepareManagerReadOnlyHost(); err != nil {
			t.Fatalf("an in-entry link must be accepted: %v", err)
		}
	})
}
