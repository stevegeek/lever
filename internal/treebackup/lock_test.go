//go:build linux || darwin

package treebackup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// holdExternally takes a flock on a store lock file through its own open
// file description, the way another lever process would.
func holdExternally(t *testing.T, s *Store, name string, how int) func() {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(s.Dir(), name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), how); err != nil {
		t.Fatal(err)
	}
	return func() { f.Close() }
}

func TestGCSkipsWhenBusy(t *testing.T) {
	s := newStore(t)
	snap(t, s, snapOpts{files: map[string]string{"a": "x"}})
	if _, err := s.GC(keepAll()); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteGCStatus(GCStatus{Result: GCBlocked, Blocked: testID, BlobCount: 1}); err != nil {
		t.Fatal(err)
	}
	release := holdExternally(t, s, lockName, unix.LOCK_SH)
	res, err := s.GC(keepAll())
	if err != nil || !res.Busy {
		t.Fatalf("GC = %+v, %v; want busy", res, err)
	}
	st, err := s.ReadGCStatus()
	if err != nil || st.Result != GCSkippedBusy || st.BlobCount != 1 || st.Blocked != testID {
		t.Fatalf("status = %+v, %v", st, err)
	}
	start := time.Now()
	if _, err := s.Delete([]string{"20200101T000000Z"}, GCOptions{Keep: Keep{Last: 1}, Wait: 100 * time.Millisecond}); !errors.Is(err, ErrBusy) {
		t.Fatalf("Delete = %v", err)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("Delete did not wait")
	}
	sh, err := s.LockShared(0)
	if err != nil {
		t.Fatalf("a shared lock beside another shared holder: %v", err)
	}
	sh.Release()
	release()
	if res, err := s.GC(keepAll()); err != nil || res.Busy {
		t.Fatalf("GC after release = %+v, %v", res, err)
	}
}

func TestSnapshotLockNoWait(t *testing.T) {
	s := newStore(t)
	release := holdExternally(t, s, snapshotLockName, unix.LOCK_EX)
	if _, err := s.LockSnapshot(0); !errors.Is(err, ErrBusy) {
		t.Fatalf("LockSnapshot = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		l, err := s.LockSnapshot(5 * time.Second)
		if err == nil {
			l.Release()
		}
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	release()
	if err := <-done; err != nil {
		t.Fatalf("waiting LockSnapshot = %v", err)
	}
}

func TestLockSharedDoesNotBlockSnapshotLock(t *testing.T) {
	s := newStore(t)
	release := holdExternally(t, s, lockName, unix.LOCK_SH)
	defer release()
	sn, err := s.LockSnapshot(0)
	if err != nil {
		t.Fatal(err)
	}
	sn.Release()
	if _, err := s.Verify(false, 0); err != nil {
		t.Fatalf("verify beside a shared holder: %v", err)
	}
	if _, err := s.Verify(true, 0); err != nil {
		t.Fatalf("verify --fix with nothing to fix: %v", err)
	}
}
