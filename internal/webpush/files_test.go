package webpush

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateKeyCreatesOnce0600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vapid.key")
	k1, err := LoadOrCreateKey(p)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode().Perm() != 0o600 || !fi.Mode().IsRegular() {
		t.Fatalf("key file %v %v, want a 0600 regular file", fi.Mode(), err)
	}
	k2, err := LoadOrCreateKey(p)
	if err != nil || !k1.Equal(k2) {
		t.Fatalf("second load gave another key (%v)", err)
	}
}

func TestReadPrivateFileRefusesUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadPrivateFile(filepath.Join(dir, "absent"), 64); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent: %v", err)
	}
	open := filepath.Join(dir, "open")
	os.WriteFile(open, []byte("x"), 0o644)
	if _, err := ReadPrivateFile(open, 64); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("0644: %v", err)
	}
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte("x"), 0o600)
	link := filepath.Join(dir, "link")
	os.Symlink(target, link)
	if _, err := ReadPrivateFile(link, 64); err == nil {
		t.Fatal("a symbolic link was followed")
	}
	os.Mkdir(filepath.Join(dir, "d"), 0o700)
	if _, err := ReadPrivateFile(filepath.Join(dir, "d"), 64); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("directory: %v", err)
	}
	big := filepath.Join(dir, "big")
	os.WriteFile(big, make([]byte, 65), 0o600)
	if _, err := ReadPrivateFile(big, 64); err == nil {
		t.Fatal("a file over max was read")
	}
	if b, err := ReadPrivateFile(target, 64); err != nil || string(b) != "x" {
		t.Fatalf("good file: %q %v", b, err)
	}
}

func TestLoadOrCreateKeyRefusesAnOpenKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vapid.key")
	if _, err := LoadOrCreateKey(p); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, 0o640)
	if _, err := LoadOrCreateKey(p); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("0640 key: %v", err)
	}
}

// TestLoadOrCreateKeyRefusesAnEmptyKey: an empty key file (a power loss
// before the write was synced) is ErrBadKey with its path, never replaced
// silently: every device would lose its notifications.
func TestLoadOrCreateKeyRefusesAnEmptyKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vapid.key")
	os.WriteFile(p, nil, 0o600)
	if _, err := LoadOrCreateKey(p); !errors.Is(err, ErrBadKey) || !strings.Contains(err.Error(), p) {
		t.Fatalf("empty key: %v", err)
	}
	if b, _ := os.ReadFile(p); len(b) != 0 {
		t.Fatal("the empty key was replaced")
	}
}

func TestWritePrivateFileReplacesALink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	os.WriteFile(victim, []byte("keep"), 0o600)
	p := filepath.Join(dir, "subscriptions.json")
	os.Symlink(victim, p)
	if err := WritePrivateFile(p, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatal("the write went through the link")
	}
	if fi, _ := os.Lstat(p); !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}
