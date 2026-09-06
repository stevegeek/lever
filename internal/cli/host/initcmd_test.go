package host

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/cli/clitest"
)

// initFixture writes a minimal valid lever.yaml + tree into a temp dir and
// chdirs there (resolveConfigPath finds lever.yaml in the CWD).
func initFixture(t *testing.T) string {
	t.Helper()
	root := initFixtureBare(t, "workspace")
	if err := os.MkdirAll(filepath.Join(root, "workspace", "workers", "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// initFixtureBare writes the lever.yaml (with tree: as given) and chdirs
// there, but creates NO tree — the state of a fresh instance before its
// first `lever init`.
func initFixtureBare(t *testing.T, tree string) string {
	t.Helper()
	root := t.TempDir()
	// llm_auth defaults to api-key (which requires broker.api_key_file to
	// exist at 0600); force subscription mode so the fixture needs no key
	// file. tree + workers are kept exactly as given in the brief.
	yaml := "name: testapp\nbackend: orbstack\ntree: " + tree + "\nbroker:\n  llm_auth: subscription\nworkers:\n  - name: scratch\n    dir: workers/scratch\n"
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

// TestInitCreatesMissingTree: the documented order runs `lever init` BEFORE
// the first `lever up`, on a fresh lever.yaml whose tree: directory need not
// exist yet. init creates it (one level, under an existing parent) and
// scaffolds into it.
func TestInitCreatesMissingTree(t *testing.T) {
	root := initFixtureBare(t, "workspace")
	out, err := clitest.Exec(t, newInitCmd())
	if err != nil {
		t.Fatalf("init on a missing tree: %v\n%s", err, out)
	}
	tree := filepath.Join(root, "workspace")
	fi, err := os.Lstat(tree)
	if err != nil || !fi.IsDir() {
		t.Fatalf("tree must be created as a real directory: fi=%v err=%v", fi, err)
	}
	for _, p := range []string{
		filepath.Join(tree, ".claude", "skills", "lever-operator", "SKILL.md"),
		filepath.Join(tree, "workers", "scratch", ".claude", "skills", "lever-agent", "SKILL.md"),
		filepath.Join(tree, "CLAUDE.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s", p)
		}
	}
}

// TestInitRefusesDanglingSymlinkTree: a tree path that is a symlink to
// nowhere is refused, not created through — creating the link's target
// would let a planted link choose where the scaffold lands (R2).
func TestInitRefusesDanglingSymlinkTree(t *testing.T) {
	root := initFixtureBare(t, "workspace")
	target := filepath.Join(root, "elsewhere")
	if err := os.Symlink(target, filepath.Join(root, "workspace")); err != nil {
		t.Fatal(err)
	}
	out, err := clitest.Exec(t, newInitCmd())
	if err == nil {
		t.Fatalf("init through a dangling symlink must fail:\n%s", out)
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("error should name the symlink: %v", err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("nothing may be created at the link target (err=%v)", err)
	}
}

// TestInitRefusesTreeWithMissingParent: only the tree directory itself is
// created, never a chain of parents — a typo'd tree: must not silently
// materialise a nested path.
func TestInitRefusesTreeWithMissingParent(t *testing.T) {
	root := initFixtureBare(t, "nested/workspace")
	out, err := clitest.Exec(t, newInitCmd())
	if err == nil {
		t.Fatalf("init with a missing tree parent must fail:\n%s", out)
	}
	if _, err := os.Lstat(filepath.Join(root, "nested")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the parent must not be created (err=%v)", err)
	}
}

// TestInitCheckDoesNotCreateTree: --check is read-only — a missing tree is a
// failed check, not something to fix.
func TestInitCheckDoesNotCreateTree(t *testing.T) {
	root := initFixtureBare(t, "workspace")
	if _, err := clitest.Exec(t, newInitCmd(), "--check"); err == nil {
		t.Fatal("check on a missing tree must fail")
	}
	if _, err := os.Lstat(filepath.Join(root, "workspace")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("--check must not create the tree (err=%v)", err)
	}
}

func TestInitScaffoldsAndIsIdempotent(t *testing.T) {
	root := initFixture(t)
	out, err := clitest.Exec(t, newInitCmd())
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	for _, p := range []string{
		filepath.Join(root, "workspace", ".claude", "skills", "lever-operator", "SKILL.md"),
		filepath.Join(root, "workspace", "workers", "scratch", ".claude", "skills", "lever-agent", "SKILL.md"),
		filepath.Join(root, "workspace", "CLAUDE.md"),
		filepath.Join(root, ".lever-state", "skills.json"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s", p)
		}
	}
	out2, err := clitest.Exec(t, newInitCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(out2), []byte("unchanged")) {
		t.Fatalf("rerun should report unchanged:\n%s", out2)
	}
}

func TestInitCheckExitsNonZeroWhenMissingAndZeroWhenCurrent(t *testing.T) {
	initFixture(t)
	if _, err := clitest.Exec(t, newInitCmd(), "--check"); err == nil {
		t.Fatal("check on unscaffolded instance must fail")
	}
	if out, err := clitest.Exec(t, newInitCmd()); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if out, err := clitest.Exec(t, newInitCmd(), "--check"); err != nil {
		t.Fatalf("check after init must pass: %v\n%s", err, out)
	}
}

func TestInitWarnsOnOwnerEditAndForceOverwrites(t *testing.T) {
	root := initFixture(t)
	if _, err := clitest.Exec(t, newInitCmd()); err != nil {
		t.Fatal(err)
	}
	op := filepath.Join(root, "workspace", ".claude", "skills", "lever-operator", "SKILL.md")
	if err := os.WriteFile(op, []byte("owner edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := clitest.Exec(t, newInitCmd())
	if err != nil {
		t.Fatalf("init with owner edit must still exit 0: %v", err)
	}
	if !bytes.Contains([]byte(out), []byte("locally modified")) {
		t.Fatalf("want owner-edit warning:\n%s", out)
	}
	if _, err := clitest.Exec(t, newInitCmd(), "--force"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(op)
	if string(b) == "owner edit" {
		t.Fatal("--force must overwrite")
	}
}

func TestInitAdoptIsMutuallyExclusiveWithForceAndCheck(t *testing.T) {
	initFixture(t)
	if _, err := clitest.Exec(t, newInitCmd(), "--adopt", "--force"); err == nil {
		t.Fatal("--adopt --force must error")
	}
	if _, err := clitest.Exec(t, newInitCmd(), "--adopt", "--check"); err == nil {
		t.Fatal("--adopt --check must error")
	}
}

func TestInitAdoptBlessesCustomizationForCheck(t *testing.T) {
	root := initFixture(t)
	if _, err := clitest.Exec(t, newInitCmd()); err != nil {
		t.Fatal(err)
	}
	op := filepath.Join(root, "workspace", ".claude", "skills", "lever-operator", "SKILL.md")
	if err := os.WriteFile(op, []byte("owner edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := clitest.Exec(t, newInitCmd(), "--check"); err == nil {
		t.Fatal("check must fail before adoption")
	}
	out, err := clitest.Exec(t, newInitCmd(), "--adopt")
	if err != nil {
		t.Fatalf("adopt: %v\n%s", err, out)
	}
	for _, want := range []string{"adopted", "current (no adoption needed)"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Fatalf("adopt output missing %q:\n%s", want, out)
		}
	}
	if out, err := clitest.Exec(t, newInitCmd(), "--check"); err != nil {
		t.Fatalf("check after adopt must pass: %v\n%s", err, out)
	}
	// Adopted file survives a plain re-run untouched.
	if _, err := clitest.Exec(t, newInitCmd()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(op); string(b) != "owner edit" {
		t.Fatal("plain init must not touch an adopted file")
	}
	// Drift past the baseline is caught again.
	if err := os.WriteFile(op, []byte("edited after adoption"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := clitest.Exec(t, newInitCmd(), "--check"); err == nil {
		t.Fatal("check must fail on drift past the adopted baseline")
	}
}
