//go:build linux || darwin

package treebackup

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "tree-backups"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type snapOpts struct {
	files      map[string]string
	created    time.Time
	incomplete bool
	quiescent  string
	pinned     bool
	rehash     bool
}

type snapResult struct {
	id       string
	blobs    map[string]BlobResult
	warnings []string
}

// snap takes a snapshot of opts.files the way the snapshot glue does: both
// locks, the dirty marker, blobs through the sink, seq, id, commit, and
// both locks released before it returns.
func snap(t *testing.T, s *Store, o snapOpts) snapResult {
	t.Helper()
	sh, err := s.LockShared(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Release()
	sn, err := s.LockSnapshot(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer sn.Release()
	was, err := s.MarkDirty()
	if err != nil {
		t.Fatal(err)
	}
	sink := s.Sink(SinkOptions{Rehash: was || o.rehash})
	res := snapResult{blobs: map[string]BlobResult{}}
	var entries []Entry
	names := make([]string, 0, len(o.files))
	for n := range o.files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		w, err := sink.NewBlob()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, o.files[n]); err != nil {
			t.Fatal(err)
		}
		br, err := w.Commit()
		if err != nil {
			t.Fatal(err)
		}
		res.blobs[n] = br
		entries = append(entries, mFileE(n, o.files[n]))
	}
	if o.incomplete {
		entries = append(entries, Entry{Path: []byte("zz-skipped"), Type: TypeFile, State: StateSkipped, Reason: ReasonRead})
	}
	seq, err := s.NextSeq()
	if err != nil {
		t.Fatal(err)
	}
	created := o.created
	if created.IsZero() {
		created = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Hour)
	}
	id, err := s.FreeID(created)
	if err != nil {
		t.Fatal(err)
	}
	h := testHeader(id)
	h.Seq, h.Created, h.Finished, h.Pinned = seq, created, created.Add(time.Second), o.pinned
	h.Quiescent = QuiescentNone
	if o.quiescent != "" {
		h.Quiescent = o.quiescent
	}
	h.Limits.Manifest = 1 << 20
	Summarize(h, entries)
	res.warnings, err = s.CommitManifest(h, entries)
	if err != nil {
		t.Fatal(err)
	}
	res.id = id
	return res
}

func ids(t *testing.T, s *Store) []string {
	t.Helper()
	infos, _, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, in := range infos {
		out = append(out, in.ID)
	}
	return out
}

func tmpNames(t *testing.T, s *Store) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(s.Dir(), tmpDir))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func blobPath(s *Store, sum string) string {
	return filepath.Join(s.Dir(), filepath.FromSlash(blobName(sum)))
}

func keepAll() GCOptions { return GCOptions{Keep: Keep{Last: 100}} }

func TestOpenStoreRefusesUnsafe(t *testing.T) {
	dir := t.TempDir()
	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{open, link, file} {
		if _, err := OpenStore(p, false); !errors.Is(err, ErrUnsafe) {
			t.Errorf("%s: err = %v, want ErrUnsafe", filepath.Base(p), err)
		}
		if _, err := OpenStore(p, true); !errors.Is(err, ErrUnsafe) {
			t.Errorf("%s with create: err = %v, want ErrUnsafe", filepath.Base(p), err)
		}
	}
	if _, err := OpenStore(filepath.Join(dir, "missing"), false); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
}

func TestStoreLayoutAndModes(t *testing.T) {
	s := newStore(t)
	r := snap(t, s, snapOpts{files: map[string]string{"a": "hello"}})
	sum := r.blobs["a"].Sum
	if sum != sumOfString("hello") || !r.blobs["a"].New {
		t.Fatalf("blob = %+v", r.blobs["a"])
	}
	modes := map[string]os.FileMode{
		".":                       0o700 | os.ModeDir,
		tmpDir:                    0o700 | os.ModeDir,
		snapshotsDir:              0o700 | os.ModeDir,
		corruptDir:                0o700 | os.ModeDir,
		blobName(sum):             0o400,
		"blobs/sha256/" + sum[:2]: 0o700 | os.ModeDir,
		manifestName(r.id):        0o600,
		seqName:                   0o600,
		lockName:                  0o600,
		snapshotLockName:          0o600,
		reserveName:               0o600,
	}
	for name, want := range modes {
		fi, err := os.Lstat(filepath.Join(s.Dir(), name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if fi.Mode() != want {
			t.Errorf("%s: mode %v, want %v", name, fi.Mode(), want)
		}
	}
	if _, err := os.Lstat(filepath.Join(s.Dir(), dirtyName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("dirty left after a commit: %v", err)
	}
	if got := tmpNames(t, s); len(got) != 0 {
		t.Errorf("tmp/ after a commit: %v", got)
	}
	rc, size, err := s.OpenBlob(sum)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" || size != 5 {
		t.Fatalf("blob = %q (%d)", b, size)
	}
	fi, _ := os.Stat(filepath.Join(s.Dir(), reserveName))
	if fi.Size() != 1<<20 {
		t.Errorf("reserve is %d bytes, want limits.manifest", fi.Size())
	}
}

func TestOpenBlobOnlyRegularFiles(t *testing.T) {
	s := newStore(t)
	sum := sumOfString("x")
	p := blobPath(s, sum)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.OpenBlob(sum); err == nil {
		t.Fatal("a link at a blob path was opened")
	}
	if _, _, err := s.OpenBlob("../../seq" + strings.Repeat("0", 55)); err == nil {
		t.Fatal("a bad digest was opened")
	}
	if s.Sink(SinkOptions{}).HasBlob(sum, 11) {
		t.Fatal("a link counts as a blob")
	}
}

func TestCrashLeavesNoSnapshotAndGCCleansTmp(t *testing.T) {
	s := newStore(t)
	commitCrash = func(stage string) error {
		if stage == "before-rename" {
			return errors.New("crash")
		}
		return nil
	}
	t.Cleanup(func() { commitCrash = nil })
	sh, _ := s.LockShared(0)
	sn, _ := s.LockSnapshot(0)
	if _, err := s.MarkDirty(); err != nil {
		t.Fatal(err)
	}
	sink := s.Sink(SinkOptions{})
	w, _ := sink.NewBlob()
	io.WriteString(w, "committed blob")
	br, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	half, _ := sink.NewBlob()
	io.WriteString(half, "half-written")
	seq, _ := s.NextSeq()
	id, _ := s.FreeID(time.Now())
	h := testHeader(id)
	h.Seq = seq
	entries := []Entry{mFileE("a", "committed blob")}
	Summarize(h, entries)
	if _, err := s.CommitManifest(h, entries); err == nil {
		t.Fatal("the crash seam did not fire")
	}
	sh.Release()
	sn.Release()
	commitCrash = nil

	if got := ids(t, s); len(got) != 0 {
		t.Fatalf("a crashed snapshot exists: %v", got)
	}
	if len(tmpNames(t, s)) < 2 {
		t.Fatalf("tmp/ = %v, want the half blob and the manifest", tmpNames(t, s))
	}
	if was, _ := s.MarkDirty(); !was {
		t.Fatal("dirty was not left behind")
	}
	res, err := s.GC(keepAll())
	if err != nil {
		t.Fatal(err)
	}
	if got := tmpNames(t, s); len(got) != 0 {
		t.Fatalf("tmp/ after GC: %v", got)
	}
	if res.BlobsRemoved != 1 {
		t.Fatalf("removed %d blobs, want the orphan", res.BlobsRemoved)
	}
	if _, err := os.Lstat(blobPath(s, br.Sum)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the orphan blob survived")
	}
	half.Abort()
}

func TestDedupHitWithWrongSizeIsRepaired(t *testing.T) {
	s := newStore(t)
	r := snap(t, s, snapOpts{files: map[string]string{"a": "hello"}})
	p := blobPath(s, r.blobs["a"].Sum)
	os.Chmod(p, 0o600)
	if err := os.WriteFile(p, []byte("hel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s.Sink(SinkOptions{}).HasBlob(r.blobs["a"].Sum, 5) {
		t.Fatal("HasBlob ignored the size")
	}
	r2 := snap(t, s, snapOpts{files: map[string]string{"a": "hello"}})
	if br := r2.blobs["a"]; !br.Repaired || br.New {
		t.Fatalf("result = %+v, want repaired", br)
	}
	b, _ := os.ReadFile(p)
	fi, _ := os.Lstat(p)
	if string(b) != "hello" || fi.Mode() != 0o400 {
		t.Fatalf("blob = %q %v", b, fi.Mode())
	}
}

// TestGarbageBlobWithDirtyIsReplaced: a crash can leave a renamed but
// unsynced blob with the right size and garbage content. Without the dirty
// marker a dedup hit trusts it; with it, the next write re-hashes and
// replaces it.
func TestGarbageBlobWithDirtyIsReplaced(t *testing.T) {
	s := newStore(t)
	r := snap(t, s, snapOpts{files: map[string]string{"a": "hello"}})
	p := blobPath(s, r.blobs["a"].Sum)
	os.Chmod(p, 0o600)
	if err := os.WriteFile(p, []byte("HELLO"), 0o600); err != nil {
		t.Fatal(err)
	}
	r2 := snap(t, s, snapOpts{files: map[string]string{"a": "hello"}})
	if br := r2.blobs["a"]; br.Repaired || br.New {
		t.Fatalf("without dirty: %+v, want a plain dedup hit", br)
	}
	if f, err := os.Create(filepath.Join(s.Dir(), dirtyName)); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	r3 := snap(t, s, snapOpts{files: map[string]string{"a": "hello"}})
	if br := r3.blobs["a"]; !br.Repaired {
		t.Fatalf("with dirty: %+v, want repaired", br)
	}
	if b, _ := os.ReadFile(p); string(b) != "hello" {
		t.Fatalf("blob = %q", b)
	}
	// Other orphans of the crash may still be garbage: the marker stays
	// until GC has re-hashed what survived its sweep.
	if _, err := os.Lstat(filepath.Join(s.Dir(), dirtyName)); err != nil {
		t.Fatal("the commit removed the marker of a crashed snapshot")
	}
	if _, err := s.GC(GCOptions{Keep: Keep{Last: 5}, Wait: time.Second}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(s.Dir(), dirtyName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("GC left the marker")
	}
}

func TestVerifyFixQuarantines(t *testing.T) {
	s := newStore(t)
	r := snap(t, s, snapOpts{files: map[string]string{"a": "hello", "b": "world"}})
	if err := os.WriteFile(filepath.Join(s.Dir(), snapshotsDir, ".DS_Store"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Verify(false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Manifests != 1 || rep.Blobs != 2 || len(rep.BadBlobs) != 0 || !slices.Equal(rep.Strays, []string{"snapshots/.DS_Store"}) {
		t.Fatalf("clean store: %+v", rep)
	}
	sum := r.blobs["a"].Sum
	p := blobPath(s, sum)
	os.Chmod(p, 0o600)
	os.WriteFile(p, []byte("HELLO"), 0o600)
	os.Remove(blobPath(s, r.blobs["b"].Sum))

	rep, err = s.Verify(false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.BadBlobs, []string{sum}) || !slices.Equal(rep.MissingBlobs, []string{r.blobs["b"].Sum}) || len(rep.Quarantined) != 0 {
		t.Fatalf("damaged store: %+v", rep)
	}
	if _, err := os.Lstat(p); err != nil {
		t.Fatal("verify without --fix moved the blob")
	}
	rep, err = s.Verify(true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Quarantined, []string{sum}) {
		t.Fatalf("quarantined = %v", rep.Quarantined)
	}
	if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the bad blob is still in place")
	}
	if b, _ := os.ReadFile(filepath.Join(s.Dir(), corruptDir, sum)); string(b) != "HELLO" {
		t.Fatalf("quarantined content = %q", b)
	}
	r2 := snap(t, s, snapOpts{files: map[string]string{"a": "hello"}})
	if !r2.blobs["a"].New {
		t.Fatalf("the next snapshot did not rewrite the blob: %+v", r2.blobs["a"])
	}
}

func TestVerifyReportsDuplicateSeqAndBadManifests(t *testing.T) {
	s := newStore(t)
	r1 := snap(t, s, snapOpts{files: map[string]string{"a": "x"}})
	snap(t, s, snapOpts{files: map[string]string{"a": "y"}})
	// A copy of the first manifest under another id has the same seq.
	b, _ := os.ReadFile(filepath.Join(s.Dir(), manifestName(r1.id)))
	dupID := "20200101T000000Z"
	b = bytes.Replace(b, []byte(r1.id), []byte(dupID), 1)
	os.WriteFile(filepath.Join(s.Dir(), manifestName(dupID)), retrailer(b), 0o600)
	bad := "20200101T000001Z"
	os.WriteFile(filepath.Join(s.Dir(), manifestName(bad)), []byte("{}\n"), 0o600)
	rep, err := s.Verify(false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.DuplicateSeq, []uint64{1}) || !slices.Equal(rep.BadManifests, []string{bad}) || rep.OK() {
		t.Fatalf("report = %+v", rep)
	}
}

func TestSeqNeverReusedAfterDelete(t *testing.T) {
	s := newStore(t)
	var last snapResult
	for i := 0; i < 3; i++ {
		last = snap(t, s, snapOpts{files: map[string]string{"a": "x"}})
	}
	if _, err := s.Delete([]string{last.id}, keepAll()); err != nil {
		t.Fatal(err)
	}
	r := snap(t, s, snapOpts{files: map[string]string{"a": "x"}})
	h, err := ValidateAll(s.Open(r.id))
	if err != nil {
		t.Fatal(err)
	}
	if h.Seq != 4 {
		t.Fatalf("seq = %d, want 4", h.Seq)
	}
}

func TestNextSeqTakesTheHighestManifestSeq(t *testing.T) {
	s := newStore(t)
	snap(t, s, snapOpts{files: map[string]string{"a": "x"}})
	snap(t, s, snapOpts{files: map[string]string{"a": "x"}})
	os.Remove(filepath.Join(s.Dir(), seqName))
	if n, err := s.NextSeq(); err != nil || n != 3 {
		t.Fatalf("seq = %d, %v; want 3", n, err)
	}
	os.WriteFile(filepath.Join(s.Dir(), seqName), []byte("ten\n"), 0o600)
	if _, err := s.NextSeq(); err == nil {
		t.Fatal("a corrupt seq file was accepted")
	}
}

func TestFreeIDAndResolve(t *testing.T) {
	s := newStore(t)
	t0 := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	a := snap(t, s, snapOpts{created: t0})
	b := snap(t, s, snapOpts{created: t0})
	c := snap(t, s, snapOpts{created: t0})
	if a.id != "20261013T090000Z" || b.id != "20261013T090000Z-2" || c.id != "20261013T090000Z-3" {
		t.Fatalf("ids = %s %s %s", a.id, b.id, c.id)
	}
	for ref, want := range map[string]string{"latest": c.id, "latest~0": c.id, "latest~2": a.id, b.id: b.id} {
		if got, err := s.Resolve(ref); err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
	for _, ref := range []string{"latest~3", "latest~-1", "latest~01", "20991231T000000Z", "../seq", "latest~"} {
		if got, err := s.Resolve(ref); err == nil {
			t.Errorf("Resolve(%q) = %q, want an error", ref, got)
		}
	}
}

func TestListOrdersBySeqAndReportsStrays(t *testing.T) {
	s := newStore(t)
	// Ids sort the other way round from seq: order must come from seq.
	a := snap(t, s, snapOpts{created: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)})
	b := snap(t, s, snapOpts{created: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)})
	os.WriteFile(filepath.Join(s.Dir(), snapshotsDir, ".DS_Store"), nil, 0o600)
	os.Mkdir(filepath.Join(s.Dir(), snapshotsDir, "20200101T000000Z.jsonl"), 0o700)
	os.WriteFile(filepath.Join(s.Dir(), manifestName("20200101T000001Z")), []byte("junk\n"), 0o600)
	infos, strays, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 || infos[0].ID != a.id || infos[1].ID != b.id || infos[2].Header != nil || infos[2].Err == nil {
		t.Fatalf("infos = %+v", infos)
	}
	if !slices.Equal(strays, []string{".DS_Store", "20200101T000000Z.jsonl"}) {
		t.Fatalf("strays = %v", strays)
	}
}

func TestCommitSelfCheckFailureCommitsNothing(t *testing.T) {
	s := newStore(t)
	id, _ := s.FreeID(time.Now())
	h := testHeader(id)
	entries := []Entry{mFileE("a", "x")}
	Summarize(h, entries)
	h.Counts.ByType[TypeFile] = 7
	_, err := s.CommitManifest(h, entries)
	if err == nil || !strings.Contains(err.Error(), "internal error, please report") {
		t.Fatalf("err = %v", err)
	}
	if got := ids(t, s); len(got) != 0 {
		t.Fatalf("committed %v", got)
	}
	if got := tmpNames(t, s); len(got) != 0 {
		t.Fatalf("tmp/ = %v", got)
	}
}

// TestManifestCommitsAfterENOSPC: the agent filled the disk through the
// tree; the manifest write fails once, the retry overwrites the reserve's
// own blocks and commits, and a new reserve is made.
func TestManifestCommitsAfterENOSPC(t *testing.T) {
	s := newStore(t)
	snap(t, s, snapOpts{files: map[string]string{"a": "x"}})
	reserve := filepath.Join(s.Dir(), reserveName)
	reserveAtRetry := true
	manifestWriteFault = func(attempt int) error {
		if attempt == 0 {
			return syscall.ENOSPC
		}
		_, err := os.Lstat(reserve)
		reserveAtRetry = err == nil
		return nil
	}
	t.Cleanup(func() { manifestWriteFault = nil })
	r := snap(t, s, snapOpts{files: map[string]string{"a": "y"}})
	if reserveAtRetry {
		t.Fatal("the retry did not write into the reserve")
	}
	if len(r.warnings) == 0 {
		t.Fatal("no warning about the full disk")
	}
	if got := ids(t, s); len(got) != 2 {
		t.Fatalf("snapshots = %v", got)
	}
	fi, _ := os.Stat(reserve)
	if fi.Size() != 1<<20 {
		t.Fatalf("reserve not restored: %d", fi.Size())
	}

	manifestWriteFault = func(int) error { return syscall.ENOSPC }
	id, _ := s.FreeID(time.Now())
	h := testHeader(id)
	Summarize(h, nil)
	if _, err := s.CommitManifest(h, nil); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("a second ENOSPC: %v", err)
	}
}

// TestLowDiskStopsBlobsButManifestCommits: the sink refuses new blobs
// under min_free or 1% of the inodes, and the manifest still commits.
func TestLowDiskStopsBlobsButManifestCommits(t *testing.T) {
	s := newStore(t)
	var free, ffree, files uint64 = 10 << 30, 1000, 1000
	statfsFunc = func(*os.File) (uint64, uint64, uint64, error) { return free, ffree, files, nil }
	t.Cleanup(func() { statfsFunc = fsFree })
	sink := s.Sink(SinkOptions{MinFree: 2 << 30})
	if err := sink.CheckSpace(); err != nil {
		t.Fatalf("plenty of space: %v", err)
	}
	free = 1 << 30
	if err := sink.CheckSpace(); !errors.Is(err, ErrDiskLow) {
		t.Fatalf("under min_free: %v", err)
	}
	free, ffree = 10<<30, 9
	if err := sink.CheckSpace(); !errors.Is(err, ErrDiskLow) {
		t.Fatalf("under 1%% of inodes: %v", err)
	}
	files, ffree = 0, 0
	if err := sink.CheckSpace(); err != nil {
		t.Fatalf("no inode count reported: %v", err)
	}
	free = 0
	r := snap(t, s, snapOpts{incomplete: true})
	h, err := ValidateAll(s.Open(r.id))
	if err != nil || h.Complete {
		t.Fatalf("manifest on a full disk: %v complete=%v", err, h != nil && h.Complete)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	s := newStore(t)
	if _, err := s.ReadSnapshotStatus(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing status: %v", err)
	}
	st := SnapshotStatus{Time: time.Date(2026, 10, 13, 1, 2, 3, 0, time.UTC), Trigger: TriggerStop, Result: "incomplete",
		Error: `entries at "a\x01"`, ID: testID, NewestID: testID}
	if err := s.WriteSnapshotStatus(st); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadSnapshotStatus()
	if err != nil || got != st {
		t.Fatalf("got %+v, %v", got, err)
	}
	g := GCStatus{Time: st.Time, Result: GCBlocked, Blocked: testID, StoreBytes: 5, BlobCount: 1}
	if err := s.WriteGCStatus(g); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadGCStatus(); err != nil || got != g {
		t.Fatalf("got %+v, %v", got, err)
	}
	fi, _ := os.Lstat(filepath.Join(s.Dir(), gcStatusName))
	if fi.Mode() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if got := tmpNames(t, s); len(got) != 0 {
		t.Fatalf("tmp/ = %v", got)
	}
}

// A seq that only the newest manifest records (its RecordSeq failed) is
// not handed out again after that manifest is deleted.
func TestDeleteKeepsTheSeqOfTheManifestsItRemoves(t *testing.T) {
	s := newStore(t)
	snap(t, s, snapOpts{files: map[string]string{"a": "x"}})
	last := snap(t, s, snapOpts{files: map[string]string{"a": "x"}})
	if err := os.Remove(filepath.Join(s.Dir(), seqName)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete([]string{last.id}, keepAll()); err != nil {
		t.Fatal(err)
	}
	if n, err := s.NextSeq(); err != nil || n != 3 {
		t.Fatalf("next seq = %d, %v; want 3", n, err)
	}
}
