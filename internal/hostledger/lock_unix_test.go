//go:build unix

package hostledger

import (
	"errors"
	"path/filepath"
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
