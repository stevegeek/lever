package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/testutil"
)

// hostPathsInstance writes a config whose tree is ws, with ws/tools (a
// read_only candidate) and ws/workers/w (a worker dir) on disk, and returns
// the config path and the instance root.
func hostPathsInstance(t *testing.T, body string) (string, string) {
	t.Helper()
	p := writeConfig(t, "name: demo\nbackend: orbstack\ntree: ws\n"+body)
	root := filepath.Dir(p)
	mustWrite(t, filepath.Join(root, "ws", "tools", "bin"))
	mustWrite(t, filepath.Join(root, "ws", "secret"))
	mustWrite(t, filepath.Join(root, "ws", "workers", "w", "bin"))
	return p, root
}

// supervisedTool is a broker block with one supervised tool running cmd.
func supervisedTool(cmd string) string {
	return "broker:\n  llm_auth: subscription\n  tools:\n    - name: t\n      backend: 127.0.0.1:3201\n      command: " + cmd + "\n"
}

func TestHostPathsRefusedInsideTree(t *testing.T) {
	cases := []struct{ name, body, key string }{
		{"credential_file", "manager:\n  credential_file: ws/secret\n", "manager.credential_file"},
		{"credential_file missing", "manager:\n  credential_file: ws/new/oauth\n", "manager.credential_file"},
		{"api_key_file", "manager: {}\nbroker:\n  llm_auth: api-key\n  api_key_file: ws/secret\n", "broker.api_key_file"},
		{"signing_key", "manager: {}\noperator:\n  signing_key: ws/secret\n", "operator.signing_key"},
		{"allowed_signers", "manager: {}\noperator:\n  allowed_signers: ws/secret\n", "operator.allowed_signers"},
		{"command", "manager: {}\n" + supervisedTool("[ws/tools/bin]"), "broker.tools[t] command"},
		{"command absolute missing", "manager: {}\n" + supervisedTool("[ROOT/ws/gone/bin]"), "broker.tools[t] command"},
		{"tree itself as state", "manager: {}\n" + supervisedTool("[/usr/bin/true, -state, ROOT/ws]"), "broker.tools[t] -state"},
		{"app-key", "manager: {}\n" + supervisedTool("[/usr/bin/true, -app-key, ROOT/ws/secret]"), "broker.tools[t] -app-key"},
		{"token-file with =", "manager: {}\n" + supervisedTool("[/usr/bin/true, --token-file=ws/secret]"), "broker.tools[t] -token-file"},
		{"fizzy cli", "manager: {}\n" + supervisedTool("[/usr/bin/true, -fizzy, ROOT/ws/tools/bin]"), "broker.tools[t] -fizzy"},
		{"script argument", "manager: {}\n" + supervisedTool("[/usr/bin/ruby, ROOT/ws/tools/bin]"), "broker.tools[t] argument"},
		{"unknown flag value", "manager: {}\n" + supervisedTool("[/usr/bin/true, -config=ws/tools/bin]"), "broker.tools[t] -config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, root := hostPathsInstance(t, "")
			body := "name: demo\nbackend: orbstack\ntree: ws\n" + strings.ReplaceAll(tc.body, "ROOT", root)
			if !strings.Contains(body, "broker:") {
				body += "broker:\n  llm_auth: subscription\n"
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadNoHostChecks(p)
			testutil.WantErrContaining(t, err, tc.key, "inside the mounted tree", "move it outside the tree")
		})
	}
}

// A link at the instance root into the tree, or a case alias of the tree on
// a case-insensitive filesystem, names agent-writable bytes all the same.
func TestHostPathsRefusedThroughSymlink(t *testing.T) {
	p, root := hostPathsInstance(t, "manager:\n  credential_file: img/secret\n")
	if err := os.Symlink("ws", filepath.Join(root, "img")); err != nil {
		t.Fatal(err)
	}
	_, err := LoadNoHostChecks(p)
	testutil.WantErrContaining(t, err, "manager.credential_file", "inside the mounted tree")
}

// A program reached through a link is refused even under read_only: the
// link is not covered by the read-only mount.
func TestHostProgramRefusedThroughLinkInTree(t *testing.T) {
	p, root := hostPathsInstance(t, "manager:\n  read_only: [tools]\n"+supervisedTool("[ws/link/bin]"))
	if err := os.Symlink("tools", filepath.Join(root, "ws", "link")); err != nil {
		t.Fatal(err)
	}
	_, err := LoadNoHostChecks(p)
	testutil.WantErrContaining(t, err, "broker.tools[t] command", "through a symbolic link")
}

// An in-tree link that points OUT is refused too: the agent can repoint it.
func TestHostPathRefusedThroughLinkOutOfTree(t *testing.T) {
	p, root := hostPathsInstance(t, "manager: {}\n"+supervisedTool("[ws/out]"))
	outside := filepath.Join(t.TempDir(), "bin")
	mustWrite(t, outside)
	if err := os.Symlink(outside, filepath.Join(root, "ws", "out")); err != nil {
		t.Fatal(err)
	}
	_, err := LoadNoHostChecks(p)
	testutil.WantErrContaining(t, err, "broker.tools[t] command", "through a symbolic link")
}

func TestHostPathsOutsideTreeAccepted(t *testing.T) {
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret"))
	mustWrite(t, filepath.Join(outside, "bin"))
	body := "manager:\n  credential_file: " + outside + "/secret\n" +
		"operator:\n  signing_key: " + outside + "/secret\n  allowed_signers: signers\n" +
		supervisedTool("["+outside+"/bin, -app-key, "+outside+"/secret, -state, "+outside+"/state, -repos, owner/name, -dsn, \"file:ref.db\"]")
	p, root := hostPathsInstance(t, body)
	mustWrite(t, filepath.Join(root, "signers"))
	// A root link to a file outside the tree is the operator's business.
	if err := os.Symlink(filepath.Join(outside, "bin"), filepath.Join(root, "bin")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadNoHostChecks(p); err != nil {
		t.Fatalf("outside paths refused: %v", err)
	}
}

// The assistant's layout: its CLI lives in the tree under a read_only entry
// that no worker dir overlaps.
func TestHostProgramUnderReadOnlyAccepted(t *testing.T) {
	p, root := hostPathsInstance(t, "manager:\n  read_only: [tools]\nworkers:\n  - {name: w, dir: workers/w}\n"+supervisedTool("[ws/tools/bin]"))
	if _, err := LoadNoHostChecks(p); err != nil {
		t.Fatalf("program under read_only refused: %v", err)
	}
	// Named absolutely, and as an interpreter's script argument.
	body := "name: demo\nbackend: orbstack\ntree: ws\nmanager:\n  read_only: [tools]\n" + supervisedTool("[/usr/bin/ruby, "+root+"/ws/tools/bin]")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadNoHostChecks(p); err != nil {
		t.Fatalf("script under read_only refused: %v", err)
	}
}

// A secret gets no read_only exception: the manager can read a read-only
// path.
func TestHostSecretUnderReadOnlyRefused(t *testing.T) {
	p, _ := hostPathsInstance(t, "manager:\n  read_only: [tools]\n  credential_file: ws/tools/bin\n")
	_, err := LoadNoHostChecks(p)
	testutil.WantErrContaining(t, err, "manager.credential_file", "still lets the manager read it")
}

// A program in the tree but under no read_only entry names the fix.
func TestHostProgramOutsideReadOnlyRefused(t *testing.T) {
	p, _ := hostPathsInstance(t, "manager:\n  read_only: [tools]\n"+supervisedTool("[ws/workers/w/bin]"))
	_, err := LoadNoHostChecks(p)
	testutil.WantErrContaining(t, err, "broker.tools[t] command", "cover it with manager.read_only")
}

// Validate already refuses a worker dir that overlaps a read_only entry;
// the host-path check refuses a program in a worker dir on its own too, in
// case that rule ever loosens.
func TestHostProgramInWorkerDirRefused(t *testing.T) {
	_, root := hostPathsInstance(t, "")
	a := &App{dir: root, Tree: filepath.Join(root, "ws"),
		Manager: Manager{ReadOnly: []string{"workers"}},
		Workers: []Worker{{Name: "w", Dir: "workers/w"}},
		Broker:  Broker{Tools: []Tool{{Name: "t", Command: []string{"ws/workers/w/bin"}}}},
	}
	testutil.WantErrContaining(t, a.checkHostPathsOutsideTree(), `worker "w"`, "read-write")
}

func TestToolHostPaths(t *testing.T) {
	got := toolHostPaths(Tool{Name: "t", Command: []string{"lever-tool-github", "-tree", "/a/ws", "-app-key", "k.pem",
		"--state=/s", "-repos", "o/n", "-branch-prefix=agent", "-fizzy", "/bin/fizzy", "plain"}})
	want := []hostPath{
		{"broker.tools[t] argument \"/a/ws\"", "/a/ws", hostProgram},
		{"broker.tools[t] -app-key", "k.pem", hostSecret},
		{"broker.tools[t] -state", "/s", hostSecret},
		{"broker.tools[t] argument \"o/n\"", "o/n", hostProgram},
		{"broker.tools[t] -fizzy", "/bin/fizzy", hostProgram},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d paths %v, want %v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("path %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if toolHostPaths(Tool{Name: "x", External: true}) != nil {
		t.Error("an external tool has no host paths")
	}
}

// On a case-insensitive filesystem WS is the tree: the walk recognises the
// tree by identity, not by spelling.
func TestHostPathsRefusedThroughCaseAlias(t *testing.T) {
	p, root := hostPathsInstance(t, "manager:\n  credential_file: WS/secret\n")
	if _, err := os.Stat(filepath.Join(root, "WS")); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	_, err := LoadNoHostChecks(p)
	testutil.WantErrContaining(t, err, "manager.credential_file", "inside the mounted tree")
}
