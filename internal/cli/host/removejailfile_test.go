//go:build unix

package host

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRemoveJailFileScriptRemovesEveryLink runs the script under sh. An agent
// can leave a symlink as the tree's marker; scion init would write through a
// dangling one, so every link goes, while a directory and its contents stay.
func TestRemoveJailFileScriptRemovesEveryLink(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	run := func(t *testing.T, p string) {
		t.Helper()
		if out, err := exec.Command(sh, "-c", removeJailFileScript, "_", p).CombinedOutput(); err != nil {
			t.Fatalf("script: %v: %s", err, out)
		}
	}
	gone := func(t *testing.T, p string) {
		t.Helper()
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still exists (lstat err %v)", p, err)
		}
	}
	kept := func(t *testing.T, p string) {
		t.Helper()
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s was removed: %v", p, err)
		}
	}

	t.Run("dangling symlink", func(t *testing.T) {
		dir := t.TempDir()
		m, target := filepath.Join(dir, ".scion"), filepath.Join(dir, "elsewhere", "planted")
		if err := os.Symlink(target, m); err != nil {
			t.Fatal(err)
		}
		run(t, m)
		gone(t, m)
		gone(t, target)
	})
	t.Run("symlink to a file", func(t *testing.T) {
		dir := t.TempDir()
		m, target := filepath.Join(dir, ".scion"), filepath.Join(dir, "f")
		if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, m); err != nil {
			t.Fatal(err)
		}
		run(t, m)
		gone(t, m)
		kept(t, target)
	})
	t.Run("symlink to a directory", func(t *testing.T) {
		dir := t.TempDir()
		m, target := filepath.Join(dir, ".scion"), filepath.Join(dir, "d")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, m); err != nil {
			t.Fatal(err)
		}
		run(t, m)
		gone(t, m)
		kept(t, target)
	})
	t.Run("regular file", func(t *testing.T) {
		m := filepath.Join(t.TempDir(), ".scion")
		if err := os.WriteFile(m, []byte("project-id: x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, m)
		gone(t, m)
	})
	t.Run("directory", func(t *testing.T) {
		m := filepath.Join(t.TempDir(), ".scion")
		if err := os.Mkdir(m, 0o700); err != nil {
			t.Fatal(err)
		}
		run(t, m)
		kept(t, m)
	})
	t.Run("absent", func(t *testing.T) {
		run(t, filepath.Join(t.TempDir(), ".scion"))
	})
}
