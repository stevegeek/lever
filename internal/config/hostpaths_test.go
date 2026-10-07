package config

import (
	"fmt"
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
		{"unknown flag value", "manager: {}\n" + supervisedTool("[/usr/bin/true, -config=ws/tools/bin]"), `broker.tools[t] argument "-config=`},
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
	type got struct {
		key, path string
		kind      hostPathKind
	}
	paths := func(cmd ...string) []got {
		var out []got
		for _, p := range toolHostPaths(Tool{Name: "t", Command: cmd}) {
			if p.tool != "t" {
				t.Errorf("%s: tool = %q, want t", p.key, p.tool)
			}
			out = append(out, got{p.key, p.path, p.kind})
		}
		return out
	}
	check := func(name string, have, want []got) {
		t.Helper()
		if len(have) != len(want) {
			t.Fatalf("%s: got %v, want %v", name, have, want)
		}
		for i := range want {
			if have[i] != want[i] {
				t.Errorf("%s: path %d = %+v, want %+v", name, i, have[i], want[i])
			}
		}
	}
	arg := func(a, p string, kind hostPathKind) got {
		return got{fmt.Sprintf("broker.tools[t] argument %q", a), p, kind}
	}
	check("shipped flags", paths("lever-tool-github", "-tree", "/a/ws", "-app-key", "k.pem", "--state=/s",
		"-repos", "o/n", "-branch-prefix=agent", "-fizzy", "/bin/fizzy", "plain"), []got{
		{"broker.tools[t] -app-key", "k.pem", hostSecret},
		{"broker.tools[t] -state", "/s", hostSecret},
		arg("o/n", "o/n", hostSecret), arg("o/n", "/n", hostSecret),
		{"broker.tools[t] -fizzy", "/bin/fizzy", hostProgram},
	})
	// An unknown flag's file is a secret: no read_only exception (LOW-1).
	check("unknown flag", paths("/opt/x", "--key-file=/k"), []got{
		{"broker.tools[t] command", "/opt/x", hostProgram},
		arg("--key-file=/k", "/k", hostSecret),
	})
	// An interpreter's first argument is the script it runs: a program.
	check("interpreter script", paths("ruby", "/t/x.rb", "/t/y"), []got{
		arg("/t/x.rb", "/t/x.rb", hostProgram), arg("/t/x.rb", "/x.rb", hostSecret),
		arg("/t/y", "/t/y", hostSecret), arg("/t/y", "/y", hostSecret),
	})
	// Glued flags and shell strings (LOW-2).
	check("glued and shell", paths("sh", "-c", "exec /t/x", "-I/l"), []got{
		arg("exec /t/x", "/t/x", hostSecret), arg("exec /t/x", "/x", hostSecret),
		arg("-I/l", "/l", hostSecret),
	})
	if toolHostPaths(Tool{Name: "x", External: true}) != nil {
		t.Error("an external tool has no host paths")
	}
}

// LOW-1: an unknown flag naming a file in the tree is a secret, refused even
// under read_only; an interpreter's script there is a program, allowed.
func TestHostPathUnknownFlagInTreeIsSecret(t *testing.T) {
	p, root := hostPathsInstance(t, "")
	write := func(cmd string) {
		body := "name: demo\nbackend: orbstack\ntree: ws\nmanager:\n  read_only: [tools]\n" + supervisedTool(strings.ReplaceAll(cmd, "ROOT", root))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("[/usr/bin/true, --key-file=ROOT/ws/tools/bin]")
	_, err := LoadNoHostChecks(p)
	testutil.WantErrContaining(t, err, "--key-file=", "still lets the manager read it")
	write("[python3, ROOT/ws/tools/bin]")
	if _, err := LoadNoHostChecks(p); err != nil {
		t.Fatalf("an interpreter's script under read_only refused: %v", err)
	}
	write("[python3, -u, ROOT/ws/tools/bin]")
	_, err = LoadNoHostChecks(p)
	testutil.WantErrContaining(t, err, "still lets the manager read it")
}

// The github tool's -tree names the tree itself on purpose (to refuse its
// key and state there); it is not a host path.
func TestHostPathGithubTreeFlagAccepted(t *testing.T) {
	outside := t.TempDir()
	p, root := hostPathsInstance(t, "")
	body := "name: demo\nbackend: orbstack\ntree: ws\nmanager: {}\n" + supervisedTool("[lever-tool-github, -tree, "+root+"/ws, -state, "+outside+"/state, -app-key, "+outside+"/key.pem, -repos, \"o/a,o/b\", -branch-prefix, agent/]")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadNoHostChecks(p); err != nil {
		t.Fatalf("the github tool's documented command refused: %v", err)
	}
}

// LOW-2: paths glued to a flag or inside a shell string are found.
func TestHostPathGluedAndShellStringRefused(t *testing.T) {
	for _, cmd := range []string{
		"[/usr/bin/ruby, -IROOT/ws/lib, /opt/x.rb]",
		"[/usr/bin/ruby, -rROOT/ws/evil.rb, /opt/x.rb]",
		"[/bin/sh, -c, \"exec ROOT/ws/tools/bin\"]",
		"[/usr/bin/true, \"-path=/opt:ROOT/ws/lib\"]",
	} {
		p, root := hostPathsInstance(t, "")
		body := "name: demo\nbackend: orbstack\ntree: ws\nmanager: {}\n" + supervisedTool(strings.ReplaceAll(cmd, "ROOT", root))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadNoHostChecks(p)
		testutil.WantErrContaining(t, err, "inside the mounted tree")
	}
}

// LOW-3: ".." in a tool path is refused, because the kernel resolves it
// after links (ws/link/../tools/bin reads as under tools).
func TestHostPathDotDotRefused(t *testing.T) {
	p, root := hostPathsInstance(t, "manager:\n  read_only: [tools]\n"+supervisedTool("[ws/workers/../tools/bin]"))
	_, err := LoadNoHostChecks(p)
	testutil.WantErrContaining(t, err, "broker.tools[t] command", `"ws/workers/../tools/bin"`, `".." component`)
	body := "name: demo\nbackend: orbstack\ntree: ws\nmanager: {}\n" + supervisedTool("[/usr/bin/true, -token-file, "+root+"/../elsewhere/tok]")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadNoHostChecks(p)
	testutil.WantErrContaining(t, err, "broker.tools[t] -token-file", `".." component`)
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
