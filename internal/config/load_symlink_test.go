package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/testutil"
)

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The tree check must follow symlinks: a link at the instance root into the
// tree (`img -> ws`) is lexically outside it, but the bytes it names are
// agent-writable all the same.
func TestLoadRejectsBootFilesReachedThroughRootSymlink(t *testing.T) {
	cases := []struct{ key, body string }{
		{"image_tar", "manager:\n  image: lever.local/x\n  image_tar: img/x.tar\n"},
		{"prompt_file", "manager:\n  prompt_file: img/boot.md\n"},
		{"instructions_file", "manager:\n  instructions_file: img/manual.md\n"},
		{"scion.binary", "manager: {}\nscion:\n  binary: img/scion\n"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			p := writeConfig(t, "name: demo\nbackend: orbstack\ntree: ws\n"+tc.body)
			root := filepath.Dir(p)
			for _, f := range []string{"x.tar", "boot.md", "manual.md", "scion"} {
				mustWrite(t, filepath.Join(root, "ws", f))
			}
			if err := os.Symlink("ws", filepath.Join(root, "img")); err != nil {
				t.Fatal(err)
			}
			_, err := LoadNoHostChecks(p)
			testutil.WantErrContaining(t, err, "inside the mounted tree", tc.key)
		})
	}
}

// A symlink at the root that points to a file OUTSIDE the tree is the
// operator's business and stays accepted.
func TestLoadAcceptsRootSymlinkToFileOutsideTree(t *testing.T) {
	p := writeConfig(t, "name: demo\nbackend: orbstack\ntree: ws\nmanager:\n  prompt_file: boot.md\n  image: lever.local/x\n  image_tar: x.tar\n")
	root := filepath.Dir(p)
	outside := t.TempDir()
	for _, f := range []string{"boot.md", "x.tar"} {
		mustWrite(t, filepath.Join(outside, f))
		if err := os.Symlink(filepath.Join(outside, f), filepath.Join(root, f)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadNoHostChecks(p); err != nil {
		t.Fatalf("a root symlink to a file outside the tree must be accepted: %v", err)
	}
}

func TestLoadRejectsDirectoryAsBootFile(t *testing.T) {
	cases := []struct{ key, body string }{
		{"prompt_file", "manager:\n  prompt_file: boot\n"},
		{"image_tar", "manager:\n  image: lever.local/x\n  image_tar: boot\n"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			p := writeConfig(t, "name: demo\nbackend: orbstack\ntree: ws\n"+tc.body)
			if err := os.Mkdir(filepath.Join(filepath.Dir(p), "boot"), 0o755); err != nil {
				t.Fatal(err)
			}
			_, err := LoadNoHostChecks(p)
			testutil.WantErrContaining(t, err, "not a regular file", tc.key)
		})
	}
}

// A boot file that does not exist yet is resolved against its deepest
// existing ancestor for the tree check; apply reports its absence.
func TestLoadMissingBootFileResolvesAgainstExistingAncestor(t *testing.T) {
	p := writeConfig(t, "name: demo\nbackend: orbstack\ntree: ws\nmanager:\n  prompt_file: img/later/boot.md\n")
	root := filepath.Dir(p)
	if err := os.Mkdir(filepath.Join(root, "ws"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("ws", filepath.Join(root, "img")); err != nil {
		t.Fatal(err)
	}
	_, err := LoadNoHostChecks(p)
	testutil.WantErrContaining(t, err, "inside the mounted tree", "prompt_file")
	// And a missing file that is genuinely outside still loads.
	p = writeConfig(t, "name: demo\nbackend: orbstack\ntree: ws\nmanager:\n  prompt_file: later/boot.md\n")
	if _, err := LoadNoHostChecks(p); err != nil {
		t.Fatalf("a missing prompt file outside the tree must load: %v", err)
	}
}
