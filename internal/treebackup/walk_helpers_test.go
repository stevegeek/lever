package treebackup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

// memSink is an in-memory BlobSink that keeps every blob's bytes by sum.
type memSink struct {
	mu       sync.Mutex
	blobs    map[string][]byte
	writes   int // committed blobs, dedup hits included
	checks   int
	lowAfter int // CheckSpace calls that pass before ErrDiskLow; <0 never
	enospc   bool
}

func newMemSink() *memSink { return &memSink{blobs: make(map[string][]byte), lowAfter: -1} }

func (s *memSink) NewBlob() (BlobWriter, error) { return &memWriter{s: s}, nil }

func (s *memSink) HasBlob(sum string, size int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.blobs[sum]
	return ok && int64(len(b)) == size
}

func (s *memSink) CheckSpace() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks++
	if s.lowAfter >= 0 && s.checks > s.lowAfter {
		return ErrDiskLow
	}
	return nil
}

func (s *memSink) has(sum string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.blobs[sum]
	return ok
}

type memWriter struct {
	s    *memSink
	buf  bytes.Buffer
	done bool
}

func (w *memWriter) Write(p []byte) (int, error) {
	if w.s.enospc {
		return 0, &os.PathError{Op: "write", Path: "tmp/blob", Err: syscall.ENOSPC}
	}
	return w.buf.Write(p)
}

func (w *memWriter) Commit() (BlobResult, error) {
	if w.done {
		panic("blob committed twice")
	}
	w.done = true
	sum := sha256.Sum256(w.buf.Bytes())
	h := hex.EncodeToString(sum[:])
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	w.s.writes++
	_, had := w.s.blobs[h]
	w.s.blobs[h] = bytes.Clone(w.buf.Bytes())
	return BlobResult{Sum: h, Size: int64(w.buf.Len()), New: !had}, nil
}

func (w *memWriter) Abort() {
	if w.done {
		panic("blob aborted after commit")
	}
	w.done = true
}

func testLimits() Limits {
	l := DefaultLimits()
	l.Time = 0
	return l
}

func walkOpts(tree string, sink *memSink) WalkOptions {
	return WalkOptions{Tree: tree, Limits: testLimits(), Sink: sink, Start: time.Now()}
}

func mustWalk(t *testing.T, o WalkOptions) *WalkResult {
	t.Helper()
	res, err := Walk(context.Background(), o)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	checkValid(t, res, o.Limits)
	return res
}

// baseOf turns a result into a base that the stat cache trusts: created an
// hour from now, with the VM off.
func baseOf(res *WalkResult) *Base {
	return &Base{
		Header:  &Header{Created: time.Now().Add(time.Hour), Quiescent: QuiescentVMOff, Complete: complete(res)},
		Entries: slices.Clone(res.Entries),
	}
}

func complete(res *WalkResult) bool {
	if res.Root.State != StateOK || res.Root.Listing != "" {
		return false
	}
	for _, e := range res.Entries {
		if e.State == StateTruncated || e.State == StateSkipped {
			return false
		}
	}
	return true
}

func find(res *WalkResult, p string) *Entry {
	for i := range res.Entries {
		if string(res.Entries[i].Path) == p {
			return &res.Entries[i]
		}
	}
	return nil
}

func mustFind(t *testing.T, res *WalkResult, p string) *Entry {
	t.Helper()
	e := find(res, p)
	if e == nil {
		t.Fatalf("no entry %q; entries: %s", p, dumpPaths(res))
	}
	return e
}

func dumpPaths(res *WalkResult) string {
	var b bytes.Buffer
	for _, e := range res.Entries {
		b.WriteString(Q(e.Path))
		b.WriteString(":" + string(e.Type))
		if e.State != "" {
			b.WriteString("/" + string(e.State))
		}
		if e.Reason != "" {
			b.WriteString("(" + string(e.Reason) + ")")
		}
		b.WriteString(" ")
	}
	return b.String()
}

// checkValid applies the manifest validator's entry rules to a result, and
// checks the result fits the manifest budget it was charged against.
func checkValid(t *testing.T, res *WalkResult, lim Limits) {
	t.Helper()
	inSet := func(r Reason, set []Reason) bool { return slices.Contains(set, r) }
	switch res.Root.State {
	case StateOK, StateUnreadable, StateListingUnstable:
		if res.Root.Reason != "" {
			t.Errorf("root: reason %q without truncated", res.Root.Reason)
		}
	case StateTruncated:
		if !inSet(res.Root.Reason, TruncatedReasons) {
			t.Errorf("root: bad truncated reason %q", res.Root.Reason)
		}
	default:
		t.Errorf("root: bad state %q", res.Root.State)
	}
	if res.Root.State == StateUnreadable && len(res.Entries) > 0 {
		t.Errorf("unreadable root with %d entries", len(res.Entries))
	}
	pathMax := min(lim.Path, MaxPathCap)
	dirs := map[string]bool{"": true}
	var total int64
	for i := range res.Entries {
		e := &res.Entries[i]
		q := Q(e.Path)
		if i > 0 && ComparePaths(res.Entries[i-1].Path, e.Path) >= 0 {
			t.Errorf("%s: not after %s", q, Q(res.Entries[i-1].Path))
		}
		if !validRel(e.Path) {
			t.Errorf("%s: invalid path", q)
		}
		if int64(len(e.Path)) > pathMax || len(BaseName(e.Path)) > MaxComponentCap {
			t.Errorf("%s: over the path limits", q)
		}
		if !dirs[string(ParentOf(e.Path))] {
			t.Errorf("%s: parent is not a recorded directory", q)
		}
		if e.Type == TypeDir {
			dirs[string(e.Path)] = true
		}
		if !slices.Contains(AllTypes, e.Type) {
			t.Errorf("%s: bad type %q", q, e.Type)
		}
		switch e.State {
		case StateSkipped:
			if !inSet(e.Reason, SkippedReasons) {
				t.Errorf("%s: bad skipped reason %q", q, e.Reason)
			}
		case StateTruncated:
			if !inSet(e.Reason, TruncatedReasons) {
				t.Errorf("%s: bad truncated reason %q", q, e.Reason)
			}
		default:
			if e.State != StateOK && !slices.Contains(AllStates, e.State) {
				t.Errorf("%s: bad state %q", q, e.State)
			}
			if e.Reason != "" {
				t.Errorf("%s: reason %q with state %q", q, e.Reason, e.State)
			}
		}
		if (e.SHA256 != "") != (e.Verified != 0) {
			t.Errorf("%s: sha256 and verified not together", q)
		}
		if e.SHA256 != "" {
			if e.Type != TypeFile {
				t.Errorf("%s: sha256 on a %s", q, e.Type)
			}
			if b, err := hex.DecodeString(e.SHA256); err != nil || len(b) != 32 || hex.EncodeToString(b) != e.SHA256 {
				t.Errorf("%s: bad sha256 %q", q, e.SHA256)
			}
		}
		if (e.HasTarget || len(e.Target) > 0) && e.Type != TypeLink {
			t.Errorf("%s: target on a %s", q, e.Type)
		}
		if len(e.Target) > MaxTargetCap {
			t.Errorf("%s: target over the cap", q)
		}
		if e.Listing != "" && (e.Type != TypeDir || e.Listing != ListingUnreadable) {
			t.Errorf("%s: bad listing %q on a %s", q, e.Listing, e.Type)
		}
		if !e.HasMeta && (e.Mode|uint32(e.Size)|uint32(e.Mtime)|uint32(e.Dev|e.Ino|e.Nlink)) != 0 {
			t.Errorf("%s: metadata without HasMeta", q)
		}
		if e.HasMeta && e.Mode > 0o7777 {
			t.Errorf("%s: mode %o has type bits", q, e.Mode)
		}
		total += maxLineBytes(e.Path, e.Target)
	}
	if total > lim.Manifest {
		t.Errorf("entries need up to %d manifest bytes, budget %d", total, lim.Manifest)
	}
}

func writeFile(t *testing.T, p string, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func resetSeams(t *testing.T) {
	t.Cleanup(func() {
		walkSeams.stat = nil
		walkSeams.beforeOpen = nil
		walkSeams.beforeList = nil
		walkSeams.listed = nil
		walkSeams.afterChunk = nil
	})
}

// Trees here are built just before they are walked, so every directory
// would fall inside the listing window. TestWalkRecentDirectoryIsListingUnstable
// restores it.
func init() { listingWindow = 0 }
