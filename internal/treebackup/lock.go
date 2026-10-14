package treebackup

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

var errWouldBlock = errors.New("lock is held")

// A flock belongs to an open file description: a second descriptor on the
// same lock file in one process is a second, independent lock that waits on
// the first, and a shared-to-exclusive conversion drops the shared lock when
// it fails. So each lock file is open at most once per process, and a lock
// is never converted.
var (
	heldMu sync.Mutex
	held   = map[string]bool{}
)

// Lock is a held flock on one of the store's lock files.
type Lock struct {
	f   *os.File
	key string
}

// Release unlocks and closes the lock file. It is safe to call twice.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := flockUnlock(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	heldMu.Lock()
	delete(held, l.key)
	heldMu.Unlock()
	return err
}

// lockPoll is how often a waiting lock retries: flock has no timeout.
const lockPoll = 20 * time.Millisecond

func (s *Store) lock(name string, exclusive bool, wait time.Duration) (*Lock, error) {
	key := s.key + "/" + name
	heldMu.Lock()
	if held[key] {
		heldMu.Unlock()
		return nil, fmt.Errorf("tree backups: %s is already held by this process", name)
	}
	held[key] = true
	heldMu.Unlock()
	ok := false
	defer func() {
		if !ok {
			heldMu.Lock()
			delete(held, key)
			heldMu.Unlock()
		}
	}()
	// O_CLOEXEC is implied: a child such as the command of `lever tree lock`
	// must not inherit the descriptor and keep the lock past our release.
	f, err := s.root.OpenFile(name, os.O_CREATE|os.O_RDWR|oNoFollow, 0o600)
	if err != nil {
		return nil, fmt.Errorf("tree backups: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		err = flockTry(f, exclusive)
		if err == nil {
			ok = true
			return &Lock{f: f, key: key}, nil
		}
		if !errors.Is(err, errWouldBlock) {
			f.Close()
			return nil, fmt.Errorf("tree backups: lock %s: %w", name, err)
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, ErrBusy
		}
		time.Sleep(min(lockPoll, time.Until(deadline)))
	}
}

// LockShared takes the store lock shared: snapshots, readers and
// `lever tree lock`. It returns ErrBusy after wait.
func (s *Store) LockShared(wait time.Duration) (*Lock, error) { return s.lock(lockName, false, wait) }

// LockExclusive takes the store lock exclusive: GC, delete and quarantine.
// A wait of 0 tries once.
func (s *Store) LockExclusive(wait time.Duration) (*Lock, error) { return s.lock(lockName, true, wait) }

// LockSnapshot takes snapshot.lock, which admits one snapshot at a time. A
// wait of 0 tries once.
func (s *Store) LockSnapshot(wait time.Duration) (*Lock, error) {
	return s.lock(snapshotLockName, true, wait)
}
