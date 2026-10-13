//go:build linux || darwin

package treebackup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

type infoSpec struct {
	created   time.Time
	complete  bool
	quiescent string
	pinned    bool
}

func makeInfos(specs []infoSpec) []ManifestInfo {
	var out []ManifestInfo
	for i, sp := range specs {
		q := sp.quiescent
		if q == "" {
			q = QuiescentNone
		}
		id := fmt.Sprintf("20260101T%06dZ", i)
		out = append(out, ManifestInfo{ID: id, Header: &Header{ID: id, Seq: uint64(i + 1), Created: sp.created,
			Complete: sp.complete, Quiescent: q, Pinned: sp.pinned}})
	}
	return out
}

func keptIndexes(infos []ManifestInfo, keep map[string]bool) []int {
	var out []int
	for i, in := range infos {
		if keep[in.ID] {
			out = append(out, i)
		}
	}
	return out
}

func TestKeepSet(t *testing.T) {
	day := func(d, h int) time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, time.UTC) }
	hourly := func(n int) []infoSpec {
		var s []infoSpec
		for i := 0; i < n; i++ {
			s = append(s, infoSpec{created: day(1, 0).Add(time.Duration(i) * time.Hour), complete: true})
		}
		return s
	}
	cases := []struct {
		name  string
		specs []infoSpec
		keep  Keep
		want  []int
	}{
		{"last", hourly(5), Keep{Last: 2}, []int{3, 4}},
		{"daily keeps the highest seq of each recent day", []infoSpec{
			{created: day(1, 1)}, {created: day(1, 2)}, {created: day(2, 1)}, {created: day(2, 5)}, {created: day(3, 1)}, {created: day(3, 2)},
		}, Keep{Last: 1, Daily: 2}, []int{3, 5}},
		{"weekly uses ISO weeks", []infoSpec{
			// 2026-10-04 is a Sunday: days 1-4 share ISO week 40, 5-11 are week 41.
			{created: day(1, 1)}, {created: day(4, 1)}, {created: day(5, 1)}, {created: day(11, 1)}, {created: day(12, 1)},
		}, Keep{Last: 1, Weekly: 2}, []int{3, 4}},
		{"weekly reaches back to older weeks", []infoSpec{
			{created: day(1, 1)}, {created: day(4, 1)}, {created: day(5, 1)}, {created: day(11, 1)}, {created: day(12, 1)},
		}, Keep{Last: 1, Weekly: 3}, []int{1, 3, 4}},
		{"newest complete survives 30 incomplete", append([]infoSpec{{created: day(1, 0), complete: true}}, func() []infoSpec {
			var s []infoSpec
			for i := 1; i <= 30; i++ {
				s = append(s, infoSpec{created: day(1, 0).Add(time.Duration(i) * time.Minute)})
			}
			return s
		}()...), Keep{Last: 3, Daily: 7, Weekly: 8}, []int{0, 28, 29, 30}},
		{"newest vm-off", []infoSpec{
			{created: day(1, 1), quiescent: QuiescentVMOff}, {created: day(1, 2), quiescent: QuiescentVMOff},
			{created: day(1, 3)}, {created: day(1, 4)},
		}, Keep{Last: 1}, []int{1, 3}},
		{"pinned", []infoSpec{{created: day(1, 1), pinned: true}, {created: day(1, 2)}, {created: day(1, 3), pinned: true}, {created: day(1, 4)}},
			Keep{Last: 1}, []int{0, 2, 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			infos := makeInfos(tc.specs)
			if got := keptIndexes(infos, KeepSet(infos, tc.keep)); !slices.Equal(got, tc.want) {
				t.Fatalf("kept %v, want %v", got, tc.want)
			}
		})
	}
}

func TestKeepSetOrdersBySeqNotClock(t *testing.T) {
	// The clock went back: the newest snapshot by seq has the oldest time.
	infos := makeInfos([]infoSpec{
		{created: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)},
		{created: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
	})
	if got := keptIndexes(infos, KeepSet(infos, Keep{Last: 1})); !slices.Equal(got, []int{1}) {
		t.Fatalf("kept %v", got)
	}
	infos = append(infos, ManifestInfo{ID: "20200101T000000Z", Err: errors.New("bad")})
	if !KeepSet(infos, Keep{Last: 1})["20200101T000000Z"] {
		t.Fatal("a manifest with an invalid header was not kept")
	}
}

// TestGCAfterSnapshotRuns: in one process, a snapshot releases both locks
// and GC then takes the exclusive lock. GC asked for while this process
// still holds the shared lock fails at once instead of converting or
// deadlocking.
func TestGCAfterSnapshotRuns(t *testing.T) {
	s := newStore(t)
	snap(t, s, snapOpts{files: map[string]string{"a": "x"}})
	sh, err := s.LockShared(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(GCOptions{Keep: Keep{Last: 1}}); err == nil || errors.Is(err, ErrBusy) {
		t.Fatalf("GC under our own shared lock: %v", err)
	}
	if _, err := s.LockShared(0); err == nil {
		t.Fatal("a second shared lock in one process was allowed")
	}
	sh.Release()
	sh.Release()

	r := snap(t, s, snapOpts{files: map[string]string{"a": "y"}})
	res, err := s.GC(GCOptions{Keep: Keep{Last: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Busy || !slices.Equal(ids(t, s), []string{r.id}) || res.BlobsRemoved != 1 || res.BlobCount != 1 || res.StoreBytes != 1 {
		t.Fatalf("result = %+v, snapshots %v", res, ids(t, s))
	}
	st, err := s.ReadGCStatus()
	if err != nil || st.Result != GCOK || st.BlobCount != 1 {
		t.Fatalf("status = %+v, %v", st, err)
	}
}

func TestGCIgnoresStraysAndSweepsOnlyBlobs(t *testing.T) {
	s := newStore(t)
	r := snap(t, s, snapOpts{files: map[string]string{"a": "keep me"}})
	ds := filepath.Join(s.Dir(), snapshotsDir, ".DS_Store")
	os.WriteFile(ds, []byte("finder"), 0o600)
	orphan := blobPath(s, sumOfString("orphan"))
	os.MkdirAll(filepath.Dir(orphan), 0o700)
	os.WriteFile(orphan, []byte("orphan"), 0o400)
	others := []string{
		filepath.Join(s.Dir(), sumsDir, r.blobs["a"].Sum[:2], "notablob"),
		filepath.Join(s.Dir(), sumsDir, "zz", strings.Repeat("a", 62)),
		filepath.Join(s.Dir(), sumsDir, r.blobs["a"].Sum[:2], strings.Repeat("A", 62)),
		filepath.Join(s.Dir(), corruptDir, sumOfString("old")),
	}
	for _, p := range others {
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.GC(keepAll())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Strays, []string{".DS_Store"}) || res.BlobsRemoved != 1 {
		t.Fatalf("result = %+v", res)
	}
	for _, p := range append(others, ds, blobPath(s, r.blobs["a"].Sum)) {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("GC removed %s", p)
		}
	}
	if _, err := os.Lstat(orphan); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the orphan blob survived")
	}
}

// TestCorruptManifestBlocksGC: one manifest that does not validate stops
// GC before anything is deleted; deleting it unblocks GC.
func TestCorruptManifestBlocksGC(t *testing.T) {
	s := newStore(t)
	var all []snapResult
	for i := 0; i < 4; i++ {
		all = append(all, snap(t, s, snapOpts{files: map[string]string{"a": fmt.Sprint("v", i)}}))
	}
	bad := all[1].id
	f, _ := os.OpenFile(filepath.Join(s.Dir(), manifestName(bad)), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("{}\n")
	f.Close()
	orphan := blobPath(s, sumOfString("orphan"))
	os.MkdirAll(filepath.Dir(orphan), 0o700)
	os.WriteFile(orphan, []byte("orphan"), 0o400)

	res, err := s.GC(GCOptions{Keep: Keep{Last: 1}})
	if !errors.Is(err, ErrInvalid) || res.Blocked != bad {
		t.Fatalf("GC = %+v, %v", res, err)
	}
	if got := ids(t, s); len(got) != 4 || res.BlobsRemoved != 0 || len(res.Deleted) != 0 {
		t.Fatalf("GC deleted something: %v %+v", got, res)
	}
	if _, err := os.Lstat(orphan); err != nil {
		t.Fatal("a blocked GC swept")
	}
	if st, _ := s.ReadGCStatus(); st.Result != GCBlocked || st.Blocked != bad {
		t.Fatalf("status = %+v", st)
	}

	res, err = s.Delete([]string{bad}, GCOptions{Keep: Keep{Last: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(t, s); !slices.Equal(got, []string{all[3].id}) {
		t.Fatalf("after delete: %v (%+v)", got, res)
	}
	if _, err := os.Lstat(orphan); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the orphan survived the delete's sweep")
	}
}

func TestCorruptHeaderBlocksGC(t *testing.T) {
	s := newStore(t)
	snap(t, s, snapOpts{files: map[string]string{"a": "x"}})
	bad := "20200101T000000Z"
	os.WriteFile(filepath.Join(s.Dir(), manifestName(bad)), []byte("not json\n"), 0o600)
	res, err := s.GC(keepAll())
	if err == nil || res.Blocked != bad {
		t.Fatalf("GC = %+v, %v", res, err)
	}
}

func TestDeleteRemovesPinnedAndChecksIDs(t *testing.T) {
	s := newStore(t)
	p := snap(t, s, snapOpts{files: map[string]string{"a": "pinned"}, pinned: true})
	snap(t, s, snapOpts{files: map[string]string{"a": "x"}})
	if _, err := s.GC(GCOptions{Keep: Keep{Last: 1}}); err != nil {
		t.Fatal(err)
	}
	if got := ids(t, s); len(got) != 2 {
		t.Fatalf("GC removed the pinned snapshot: %v", got)
	}
	for _, bad := range [][]string{{"../seq"}, {"20200101T000000Z"}, {p.id, "x"}} {
		if _, err := s.Delete(bad, keepAll()); err == nil {
			t.Fatalf("Delete(%q) succeeded", bad)
		}
	}
	if got := ids(t, s); len(got) != 2 {
		t.Fatalf("a refused delete removed something: %v", got)
	}
	res, err := s.Delete([]string{p.id}, keepAll())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Deleted, []string{p.id}) || res.BlobsRemoved != 1 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := s.GC(GCOptions{}); err == nil {
		t.Fatal("keep.last 0 was accepted")
	}
}

func TestGCDeletesExpiredSnapshots(t *testing.T) {
	s := newStore(t)
	var all []string
	for i := 0; i < 5; i++ {
		all = append(all, snap(t, s, snapOpts{files: map[string]string{"a": fmt.Sprint(i)},
			created: time.Date(2026, 10, 1, i, 0, 0, 0, time.UTC)}).id)
	}
	res, err := s.GC(GCOptions{Keep: Keep{Last: 2}})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(res.Deleted)
	if !slices.Equal(res.Deleted, all[:3]) || res.BlobsRemoved != 3 {
		t.Fatalf("result = %+v", res)
	}
}

func TestBloomHasNoFalseNegatives(t *testing.T) {
	bf := newBloom(10000)
	fp := 0
	for i := 0; i < 10000; i++ {
		bf.add(sumOfString(fmt.Sprint(i)))
	}
	for i := 0; i < 10000; i++ {
		if !bf.has(sumOfString(fmt.Sprint(i))) {
			t.Fatalf("false negative for %d", i)
		}
		if bf.has(sumOfString(fmt.Sprint("other", i))) {
			fp++
		}
	}
	if fp > 300 {
		t.Fatalf("%d false positives in 10000, want about 100", fp)
	}
}

func TestGCWithoutSnapshotsDirSweepsNothing(t *testing.T) {
	s := newStore(t)
	r := snap(t, s, snapOpts{files: map[string]string{"a": "x"}})
	if err := os.Rename(filepath.Join(s.Dir(), snapshotsDir), filepath.Join(s.Dir(), "moved")); err != nil {
		t.Fatal(err)
	}
	res, err := s.GC(keepAll())
	if err == nil || res.Blocked != snapshotsDir || res.BlobsRemoved != 0 {
		t.Fatalf("GC = %+v, %v", res, err)
	}
	if _, err := os.Lstat(blobPath(s, r.blobs["a"].Sum)); err != nil {
		t.Fatal("GC swept a blob with snapshots/ missing")
	}
}
