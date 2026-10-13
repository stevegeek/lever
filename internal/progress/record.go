package progress

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/stevegeek/lever/internal/termsafe"
)

// keepRecords is how many run records RecordPath keeps in its directory.
const keepRecords = 10

// RecordPath returns a new run record's path in dir
// (bringup-<command>-<UTC time>.jsonl), creating dir (0700) and removing
// the oldest records beyond keepRecords.
func RecordPath(dir, command string, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("progress records: %w", err)
	}
	name := fmt.Sprintf("bringup-%s-%s-%d.jsonl", command, now.UTC().Format("20060102T150405Z"), os.Getpid())
	prune(dir, keepRecords-1)
	return filepath.Join(dir, name), nil
}

// prune removes the oldest bringup-*.jsonl records in dir, keeping keep.
func prune(dir string, keep int) {
	matches, err := filepath.Glob(filepath.Join(dir, "bringup-*.jsonl"))
	if err != nil || len(matches) <= keep {
		return
	}
	type rec struct {
		path string
		mod  time.Time
	}
	var recs []rec
	for _, m := range matches {
		fi, err := os.Lstat(m)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		recs = append(recs, rec{m, fi.ModTime()})
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].mod.Before(recs[j].mod) })
	for len(recs) > keep {
		os.Remove(recs[0].path)
		recs = recs[1:]
	}
}

// WriteLast saves sum as the last run's summary (path, mode 0600), written
// to a temp file and renamed into place.
func WriteLast(path string, sum Summary) error {
	b, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".last-bringup-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ReadLast reads the summary WriteLast saved. A missing file is ErrNoRecord.
func ReadLast(path string) (Summary, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Summary{}, ErrNoRecord
	}
	if err != nil {
		return Summary{}, err
	}
	var sum Summary
	if err := json.Unmarshal(b, &sum); err != nil {
		return Summary{}, fmt.Errorf("%s: %w", path, err)
	}
	sum.Total = time.Duration(sum.TotalMS) * time.Millisecond
	return sum, nil
}

// Describe is the summary in one line for doctor: when, how long, the
// slowest steps.
func (s Summary) Describe(now time.Time) string {
	ago := now.Sub(s.At).Round(time.Minute)
	when := "just now"
	if ago >= time.Minute {
		h, m := int(ago.Hours()), int(ago.Minutes())%60
		switch {
		case h == 0:
			when = fmt.Sprintf("%dm ago", m)
		case m == 0:
			when = fmt.Sprintf("%dh ago", h)
		default:
			when = fmt.Sprintf("%dh%dm ago", h, m)
		}
	}
	// The labels come back from a file: show them as text only.
	return termsafe.Sanitize(fmt.Sprintf("%s (%s)", s.Line(), when))
}
