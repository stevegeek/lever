package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stevegeek/lever/internal/testutil"
)

// sharedApp is a valid App with workers alpha (workers/w) and beta
// (workers/v), and the given shared folders.
func sharedApp(t *testing.T, folders ...SharedFolder) *App {
	t.Helper()
	a := testApp(t, "workers/w", "workers/v")
	a.SharedFolders = folders
	return a
}

func TestValidateAcceptsSharedFolders(t *testing.T) {
	app := sharedApp(t,
		SharedFolder{Path: "tools/releases", Writers: []string{"alpha"}, Readers: []string{"*"}},
		SharedFolder{Path: "refs", Name: "reference", Readers: []string{"beta"}},
		SharedFolder{Path: "out", Writers: []string{"demo"}, Readers: []string{"alpha"}},
	)
	if err := app.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	body := "name: demo\nbackend: orbstack\ntree: ws\nworkers:\n  - name: alpha\n    dir: workers/w\nshared_folders:\n  - path: tools/releases\n    writers: [alpha]\n    readers: [\"*\"]\n"
	loaded, err := LoadNoHostChecks(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []SharedFolder{{Path: "tools/releases", Writers: []string{"alpha"}, Readers: []string{"*"}}}
	if !reflect.DeepEqual(loaded.SharedFolders, want) {
		t.Fatalf("shared_folders = %+v, want %+v", loaded.SharedFolders, want)
	}
}

func TestValidateRejectsBadSharedFolders(t *testing.T) {
	for _, tc := range []struct {
		name     string
		folders  []SharedFolder
		readOnly []string
		want     []string
	}{
		{"no path", []SharedFolder{{Readers: []string{"*"}}}, nil, []string{"shared_folders", "no path"}},
		{"absolute", []SharedFolder{{Path: "/etc", Readers: []string{"*"}}}, nil, []string{"shared_folders", `"/etc"`}},
		{"dot", []SharedFolder{{Path: ".", Readers: []string{"*"}}}, nil, []string{"shared_folders", `"."`}},
		{"escape", []SharedFolder{{Path: "../x", Readers: []string{"*"}}}, nil, []string{"shared_folders", "../x"}},
		{"trailing slash", []SharedFolder{{Path: "x/", Readers: []string{"*"}}}, nil, []string{"shared_folders"}},
		{"dollar", []SharedFolder{{Path: "$HOME", Readers: []string{"*"}}}, nil, []string{"shared_folders", "$VAR"}},
		{"non-ASCII", []SharedFolder{{Path: "café", Readers: []string{"*"}}}, nil, []string{"shared_folders", "ASCII"}},
		{"bad name", []SharedFolder{{Path: "x", Name: "A B", Readers: []string{"*"}}}, nil, []string{"mount name", "A B"}},
		{"bad default name", []SharedFolder{{Path: "tools/Releases", Readers: []string{"*"}}}, nil, []string{"mount name", "Releases", "name:"}},
		{"same name", []SharedFolder{{Path: "a/rel", Readers: []string{"*"}}, {Path: "b/rel", Readers: []string{"*"}}}, nil, []string{"/shared/rel", "name:"}},
		{"nested", []SharedFolder{{Path: "a", Readers: []string{"*"}}, {Path: "a/b", Readers: []string{"*"}}}, nil, []string{"overlap", `"a"`, `"a/b"`}},
		{"nested by case", []SharedFolder{{Path: "a", Readers: []string{"*"}}, {Path: "A/b", Name: "b2", Readers: []string{"*"}}}, nil, []string{"overlap"}},
		{"over read_only", []SharedFolder{{Path: "tools", Readers: []string{"*"}}}, []string{"tools/bin"}, []string{"manager.read_only", `"tools/bin"`}},
		{"over worker dir", []SharedFolder{{Path: "workers", Readers: []string{"*"}}}, nil, []string{`worker "alpha"`, "read-write"}},
		{"inside worker dir", []SharedFolder{{Path: "workers/w/out", Readers: []string{"*"}}}, nil, []string{`worker "alpha"`}},
		{"file exchange", []SharedFolder{{Path: ".lever-files", Readers: []string{"*"}}}, nil, []string{".lever-files"}},
		{"nobody", []SharedFolder{{Path: "x"}}, nil, []string{"no writers and no readers"}},
		{"unknown writer", []SharedFolder{{Path: "x", Writers: []string{"ghost"}}}, nil, []string{"writer", `"ghost"`}},
		{"star writer", []SharedFolder{{Path: "x", Writers: []string{"*"}}}, nil, []string{"writers must name each agent"}},
		{"unknown reader", []SharedFolder{{Path: "x", Readers: []string{"ghost"}}}, nil, []string{"reader", `"ghost"`}},
		{"star plus names", []SharedFolder{{Path: "x", Readers: []string{"*", "beta"}}}, nil, []string{"list it alone"}},
		{"writer and reader", []SharedFolder{{Path: "x", Writers: []string{"alpha"}, Readers: []string{"alpha"}}}, nil, []string{"both writer and reader"}},
		{"writer twice", []SharedFolder{{Path: "x", Writers: []string{"alpha", "alpha"}}}, nil, []string{"writer", "twice"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := sharedApp(t, tc.folders...)
			app.Manager.ReadOnly = tc.readOnly
			testutil.WantErrContaining(t, app.Validate(), tc.want...)
		})
	}
}

func TestSharedMountsFor(t *testing.T) {
	app := sharedApp(t,
		SharedFolder{Path: "tools/releases", Writers: []string{"alpha"}, Readers: []string{"*"}},
		SharedFolder{Path: "refs", Name: "reference", Readers: []string{"beta"}},
		SharedFolder{Path: "out", Writers: []string{"demo"}, Readers: []string{"alpha"}},
	)
	for _, tc := range []struct {
		agent string
		want  []SharedMount
	}{
		{"alpha", []SharedMount{
			{Name: "releases", Rel: "tools/releases", Target: "/shared/releases"},
			{Name: "out", Rel: "out", Target: "/shared/out", ReadOnly: true},
		}},
		{"beta", []SharedMount{
			{Name: "releases", Rel: "tools/releases", Target: "/shared/releases", ReadOnly: true},
			{Name: "reference", Rel: "refs", Target: "/shared/reference", ReadOnly: true},
		}},
		// The manager reads every folder it does not write.
		{"demo", []SharedMount{
			{Name: "releases", Rel: "tools/releases", Target: "/shared/releases", ReadOnly: true},
			{Name: "reference", Rel: "refs", Target: "/shared/reference", ReadOnly: true},
			{Name: "out", Rel: "out", Target: "/shared/out"},
		}},
		{"stranger", nil},
	} {
		if got := app.SharedMountsFor(tc.agent); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("SharedMountsFor(%q) = %+v, want %+v", tc.agent, got, tc.want)
		}
	}
}

// A shared folder the manager does not write is planned like a read_only
// entry: mounted read-only over itself, its ancestors and every worker dir
// pinned. One the manager writes adds nothing to the tree plan.
func TestManagerTreeMountsIncludeSharedFolders(t *testing.T) {
	app := sharedApp(t,
		SharedFolder{Path: "tools/releases", Writers: []string{"alpha"}, Readers: []string{"*"}},
		SharedFolder{Path: "out", Writers: []string{"demo"}, Readers: []string{"alpha"}},
	)
	want := []TreeMount{
		{Rel: "tools", ReadOnly: false},
		{Rel: "workers", ReadOnly: false, WorkerPin: true},
		{Rel: "tools/releases", ReadOnly: true},
		{Rel: "workers/v", ReadOnly: false, WorkerPin: true},
		{Rel: "workers/w", ReadOnly: false, WorkerPin: true},
	}
	if got := app.ManagerTreeMounts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ManagerTreeMounts = %+v, want %+v", got, want)
	}
	if got, want := app.ProtectedDirs(), []string{"tools/releases", "out"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ProtectedDirs = %q, want %q", got, want)
	}
	only := sharedApp(t, SharedFolder{Path: "out", Writers: []string{"demo"}, Readers: []string{"alpha"}})
	if got := only.ManagerTreeMounts(); got != nil {
		t.Fatalf("manager-written folder: ManagerTreeMounts = %+v, want none", got)
	}
}

func TestPrepareSharedFoldersHost(t *testing.T) {
	app := sharedApp(t, SharedFolder{Path: "tools/releases", Writers: []string{"alpha"}, Readers: []string{"*"}})
	testutil.WantErrContaining(t, app.PrepareSharedFoldersHost(), "tools/releases", "create it as a real directory")
	if err := os.MkdirAll(filepath.Join(app.Tree, "tools", "releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A link inside the folder is the writer's business: not refused.
	if err := os.Symlink("/etc", filepath.Join(app.Tree, "tools", "releases", "current")); err != nil {
		t.Fatal(err)
	}
	if err := app.PrepareSharedFoldersHost(); err != nil {
		t.Fatalf("PrepareSharedFoldersHost: %v", err)
	}
	if err := app.PrepareManagerReadOnlyHost(); err != nil {
		t.Fatalf("PrepareManagerReadOnlyHost: %v", err)
	}
	// A link on the path is refused, by both.
	if err := os.RemoveAll(filepath.Join(app.Tree, "tools")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(app.Tree, "elsewhere", "releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(app.Tree, "tools")); err != nil {
		t.Fatal(err)
	}
	testutil.WantErrContaining(t, app.PrepareSharedFoldersHost(), "symbolic link")
	testutil.WantErrContaining(t, app.PrepareManagerReadOnlyHost(), "symbolic link")
}

// A host program inside a shared folder is refused: a writer can change it,
// and no shared folder excuses one the way manager.read_only does.
func TestHostProgramInSharedFolderRefused(t *testing.T) {
	p, _ := hostPathsInstance(t, "workers:\n  - name: alpha\n    dir: workers/w\nshared_folders:\n  - path: tools\n    writers: [alpha]\n"+supervisedTool("[ws/tools/bin]"))
	_, err := LoadNoHostChecks(p)
	testutil.WantErrContaining(t, err, "broker.tools[t] command", "inside the mounted tree")
}
