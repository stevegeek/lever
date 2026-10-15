//go:build unix

package hostledger

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestTryLockFile: a second holder is refused with ErrLocked until the first
// unlocks (flock locks belong to the open file, so one process can test it).
func TestTryLockFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := TryLockFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLockFile(p); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v, want ErrLocked", err)
	}
	unlock()
	again, err := TryLockFile(p)
	if err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	again()
}

// TestTryLockFileAs: the holder is readable while the lock is held, refuses
// TryLockFile too, and is gone once released.
func TestTryLockFileAs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := TryLockFileAs(p, "lever up (pid 1)")
	if err != nil {
		t.Fatal(err)
	}
	if got := LockHolder(p); got != "lever up (pid 1)" {
		t.Fatalf("holder = %q", got)
	}
	if _, err := TryLockFile(p); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	if _, err := TryLockFileAs(p, "other"); !errors.Is(err, ErrLocked) {
		t.Fatalf("second holder: %v", err)
	}
	if got := LockHolder(p); got != "lever up (pid 1)" {
		t.Fatalf("a refused holder wrote: %q", got)
	}
	unlock()
	if got := LockHolder(p); got != "" {
		t.Fatalf("holder after release = %q", got)
	}
	long, err := TryLockFileAs(p, strings.Repeat("x", 1000))
	if err != nil {
		t.Fatal(err)
	}
	defer long()
	if got := LockHolder(p); len(got) != maxLockHolder {
		t.Fatalf("holder is %d bytes, want %d", len(got), maxLockHolder)
	}
}
