package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// initFixture writes a minimal valid lever.yaml + tree into a temp dir and
// chdirs there (resolveConfigPath finds lever.yaml in the CWD).
func initFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	tree := filepath.Join(root, "workspace")
	if err := os.MkdirAll(filepath.Join(tree, "groves", "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	// llm_auth defaults to api-key (which requires broker.api_key_file to
	// exist at 0600); force subscription mode so the fixture needs no key
	// file. tree + groves are kept exactly as given in the brief.
	yaml := "name: testapp\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\nworkers:\n  - name: scratch\n    dir: groves/scratch\n"
	if err := os.WriteFile(filepath.Join(root, "lever.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	return root
}

func runInit(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newInitCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestInitScaffoldsAndIsIdempotent(t *testing.T) {
	root := initFixture(t)
	out, err := runInit(t)
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	for _, p := range []string{
		filepath.Join(root, "workspace", ".claude", "skills", "lever-operator", "SKILL.md"),
		filepath.Join(root, "workspace", "groves", "scratch", ".claude", "skills", "lever-agent", "SKILL.md"),
		filepath.Join(root, "workspace", "CLAUDE.md"),
		filepath.Join(root, ".lever-state", "skills.json"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s", p)
		}
	}
	out2, err := runInit(t)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(out2), []byte("unchanged")) {
		t.Fatalf("rerun should report unchanged:\n%s", out2)
	}
}

func TestInitCheckExitsNonZeroWhenMissingAndZeroWhenCurrent(t *testing.T) {
	initFixture(t)
	if _, err := runInit(t, "--check"); err == nil {
		t.Fatal("check on unscaffolded instance must fail")
	}
	if out, err := runInit(t); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if out, err := runInit(t, "--check"); err != nil {
		t.Fatalf("check after init must pass: %v\n%s", err, out)
	}
}

func TestInitWarnsOnOwnerEditAndForceOverwrites(t *testing.T) {
	root := initFixture(t)
	if _, err := runInit(t); err != nil {
		t.Fatal(err)
	}
	op := filepath.Join(root, "workspace", ".claude", "skills", "lever-operator", "SKILL.md")
	if err := os.WriteFile(op, []byte("owner edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runInit(t)
	if err != nil {
		t.Fatalf("init with owner edit must still exit 0: %v", err)
	}
	if !bytes.Contains([]byte(out), []byte("locally modified")) {
		t.Fatalf("want owner-edit warning:\n%s", out)
	}
	if _, err := runInit(t, "--force"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(op)
	if string(b) == "owner edit" {
		t.Fatal("--force must overwrite")
	}
}
