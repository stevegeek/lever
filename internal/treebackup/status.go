package treebackup

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	snapshotStatusName = "status-snapshot.json"
	gcStatusName       = "status-gc.json"
	maxStatusBytes     = 64 << 10
)

// SnapshotStatus is the last snapshot attempt. Result is complete,
// incomplete, failed or skipped; Error is already rendered for a terminal.
// NewestID is the newest committed snapshot.
type SnapshotStatus struct {
	Time     time.Time `json:"time"`
	Trigger  string    `json:"trigger"`
	Result   string    `json:"result"`
	Error    string    `json:"error,omitempty"`
	ID       string    `json:"id,omitempty"`
	NewestID string    `json:"newest_id,omitempty"`
}

// GCStatus is the last GC. Result is ok, skipped-busy or blocked; Blocked
// names the manifest that blocked it.
type GCStatus struct {
	Time       time.Time `json:"time"`
	Result     string    `json:"result"`
	Blocked    string    `json:"blocked,omitempty"`
	StoreBytes int64     `json:"store_bytes"`
	BlobCount  int64     `json:"blob_count"`
}

// GC results.
const (
	GCOK          = "ok"
	GCSkippedBusy = "skipped-busy"
	GCBlocked     = "blocked"
)

// WriteSnapshotStatus replaces status-snapshot.json. The caller holds
// snapshot.lock.
func (s *Store) WriteSnapshotStatus(st SnapshotStatus) error {
	return s.writeStatus(snapshotStatusName, st)
}

// ReadSnapshotStatus reads status-snapshot.json; a missing file is an
// error satisfying errors.Is(err, fs.ErrNotExist).
func (s *Store) ReadSnapshotStatus() (SnapshotStatus, error) {
	var st SnapshotStatus
	return st, s.readStatus(snapshotStatusName, &st)
}

// WriteGCStatus replaces status-gc.json.
func (s *Store) WriteGCStatus(st GCStatus) error { return s.writeStatus(gcStatusName, st) }

// ReadGCStatus reads status-gc.json, like ReadSnapshotStatus.
func (s *Store) ReadGCStatus() (GCStatus, error) {
	var st GCStatus
	return st, s.readStatus(gcStatusName, &st)
}

func (s *Store) writeStatus(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return s.replaceFile(name, append(b, '\n'))
}

func (s *Store) readStatus(name string, v any) error {
	b, err := s.readSmall(name, maxStatusBytes)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("tree backups: %s: %w", name, err)
	}
	return nil
}
