package treebackup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"
	"unicode/utf8"
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
	// LastOK is the last GC that finished.
	LastOK time.Time `json:"last_ok,omitzero"`
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

// statusSlot is the size every status file is padded to (JSON allows the
// trailing spaces), so that on a full disk the next status can overwrite
// it in place.
const statusSlot = 4 << 10

// maxStatusError bounds the error a status records: escaped, it must still
// fit the slot.
const maxStatusError = 512

func (s *Store) writeStatus(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if len(b) < statusSlot {
		b = append(b, bytes.Repeat([]byte{' '}, statusSlot-len(b))...)
	}
	err = s.replaceFile(name, b)
	if isNoSpace(err) {
		if ierr := s.overwriteInPlace(name, b); ierr == nil {
			return nil
		}
	}
	return err
}

// overwriteInPlace writes b over the existing file name, which must be at
// least as long, padding b to its length. It is the full-disk fallback of
// writeStatus: not atomic, but it needs no new blocks on a filesystem that
// overwrites in place.
func (s *Store) overwriteInPlace(name string, b []byte) error {
	f, err := s.root.OpenFile(name, os.O_WRONLY|oNoFollow, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Size() < int64(len(b)) || fi.Size() > maxStatusBytes {
		return fmt.Errorf("tree backups: %s cannot hold the new status in place", name)
	}
	b = append(b, bytes.Repeat([]byte{' '}, int(fi.Size())-len(b))...)
	if _, err := f.WriteAt(b, 0); err != nil {
		return err
	}
	return fsync(f)
}

// clipError keeps a status error within maxStatusError bytes, cut on a
// rune boundary.
func clipError(s string) string {
	if len(s) <= maxStatusError {
		return s
	}
	n := maxStatusError - len("…")
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
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
