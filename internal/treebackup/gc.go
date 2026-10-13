package treebackup

import (
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"io/fs"
	"math"
	"os"
	"slices"
	"time"
)

// Keep is the retention policy: the Last highest seqs, plus the newest
// snapshot of each of the Daily most recent UTC days and the Weekly most
// recent ISO weeks that have snapshots.
type Keep struct{ Last, Daily, Weekly int }

// GCOptions configure GC and Delete. A Wait of 0 tries the lock once.
type GCOptions struct {
	Keep Keep
	Wait time.Duration
	Now  func() time.Time
}

// GCResult is what a GC or Delete did. Busy means the lock was held and
// nothing was done; Blocked names the manifest that stopped it.
type GCResult struct {
	Deleted      []string
	Blocked      string
	Strays       []string
	BlobsRemoved int64
	StoreBytes   int64
	BlobCount    int64
	Busy         bool
	// Quarantined are blobs moved to blobs/corrupt/ after a crash.
	Quarantined []string
}

func (o *GCOptions) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

// GC removes expired snapshots and the blobs no kept snapshot references.
// It fails closed: one manifest that does not validate stops it before
// anything is deleted, and the error names it (Blocked is set as well).
// When the lock is busy it records skipped-busy and returns Busy.
func (s *Store) GC(o GCOptions) (*GCResult, error) {
	if o.Keep.Last < 1 {
		return nil, errors.New("tree backups: keep.last must be at least 1")
	}
	l, err := s.LockExclusive(o.Wait)
	if errors.Is(err, ErrBusy) {
		return &GCResult{Busy: true}, s.writeGC(o, GCStatus{Result: GCSkippedBusy}, true)
	}
	if err != nil {
		return nil, err
	}
	defer l.Release()
	// No writer can be active under the exclusive lock, so everything in
	// tmp/ is left over from a crash.
	if err := s.cleanTmp(); err != nil {
		return nil, err
	}
	res := &GCResult{}
	return res, s.collect(res, o)
}

// Delete removes the named manifests, pinned or corrupt ones included,
// then validates the rest, applies retention and sweeps like GC. Every id
// must name an existing manifest file, or nothing is removed.
func (s *Store) Delete(ids []string, o GCOptions) (*GCResult, error) {
	if o.Keep.Last < 1 {
		return nil, errors.New("tree backups: keep.last must be at least 1")
	}
	for _, id := range ids {
		if !ValidID(id) {
			return nil, fmt.Errorf("tree backups: bad snapshot id %q", id)
		}
	}
	l, err := s.LockExclusive(o.Wait)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	for _, id := range ids {
		fi, err := s.root.Lstat(manifestName(id))
		if err != nil {
			return nil, fmt.Errorf("tree backups: snapshot %s: %w", id, err)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("tree backups: snapshot %s is not a regular file", id)
		}
	}
	// The manifests about to go may hold the only record of the highest
	// seq (its RecordSeq can have failed after the commit).
	next, err := s.NextSeq()
	if err != nil {
		return nil, err
	}
	if err := s.RecordSeq(next - 1); err != nil {
		return nil, fmt.Errorf("tree backups: recording the sequence number before a delete: %w", err)
	}
	res := &GCResult{}
	for _, id := range slices.Compact(slices.Sorted(slices.Values(ids))) {
		if err := s.root.Remove(manifestName(id)); err != nil {
			return res, fmt.Errorf("tree backups: %w", err)
		}
		res.Deleted = append(res.Deleted, id)
	}
	if err := s.syncSubdir(snapshotsDir); err != nil {
		return res, fmt.Errorf("tree backups: %w", err)
	}
	return res, s.collect(res, o)
}

func (s *Store) writeGC(o GCOptions, st GCStatus, keepSizes bool) error {
	prev, perr := s.ReadGCStatus()
	if perr == nil {
		st.LastOK = prev.LastOK
	}
	if st.Result == GCOK {
		st.LastOK = o.now()
	}
	if keepSizes {
		if perr == nil {
			st.StoreBytes, st.BlobCount = prev.StoreBytes, prev.BlobCount
			// A skipped run proves nothing about the manifest that blocked
			// the last one, and doctor must keep naming it.
			if st.Blocked == "" {
				st.Blocked = prev.Blocked
			}
		}
	}
	st.Time = o.now()
	return s.WriteGCStatus(st)
}

func (s *Store) cleanTmp() error {
	d, err := s.root.Open(tmpDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("tree backups: %w", err)
	}
	defer d.Close()
	for {
		names, err := d.Readdirnames(1024)
		for _, n := range names {
			if rerr := s.root.RemoveAll(tmpDir + "/" + n); rerr != nil {
				return fmt.Errorf("tree backups: clean tmp: %w", rerr)
			}
		}
		if errors.Is(err, io.EOF) || (err == nil && len(names) == 0) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tree backups: clean tmp: %w", err)
		}
	}
}

// collect is GC from validation on: the caller holds the exclusive lock.
func (s *Store) collect(res *GCResult, o GCOptions) error {
	block := func(id string, cause error) error {
		res.Blocked = id
		if err := s.writeGC(o, GCStatus{Result: GCBlocked, Blocked: id}, true); err != nil {
			return err
		}
		return fmt.Errorf("tree backups: GC blocked by %s: %w", id, cause)
	}
	// Without snapshots/ every blob looks unreferenced.
	if fi, err := s.root.Lstat(snapshotsDir); err != nil || !fi.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		return block(snapshotsDir, err)
	}
	infos, strays, err := s.List()
	if err != nil {
		return err
	}
	res.Strays = strays
	for _, in := range infos {
		if in.Header == nil {
			return block(in.ID, in.Err)
		}
		if _, err := ValidateAll(s.Open(in.ID)); err != nil {
			return block(in.ID, err)
		}
	}

	keep := KeepSet(infos, o.Keep)
	var kept []ManifestInfo
	for _, in := range infos {
		if keep[in.ID] {
			kept = append(kept, in)
			continue
		}
		if err := s.root.Remove(manifestName(in.ID)); err != nil {
			return fmt.Errorf("tree backups: %w", err)
		}
		res.Deleted = append(res.Deleted, in.ID)
	}
	if err := s.syncSubdir(snapshotsDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("tree backups: %w", err)
	}

	var refs int64
	for _, in := range kept {
		refs += in.Header.Counts.ByType[TypeFile]
	}
	bf := newBloom(refs)
	for _, in := range kept {
		if err := s.markManifest(in.ID, bf, nil); err != nil {
			return block(in.ID, err)
		}
	}

	err = s.walkBlobs(func(sum string, size int64) error {
		if bf.has(sum) {
			res.StoreBytes += size
			res.BlobCount++
			return nil
		}
		if err := s.root.Remove(blobName(sum)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		res.BlobsRemoved++
		return nil
	}, nil)
	if err != nil {
		return fmt.Errorf("tree backups: sweep: %w", err)
	}
	if err := s.clearCrash(res); err != nil {
		return err
	}
	return s.writeGC(o, GCStatus{Result: GCOK, StoreBytes: res.StoreBytes, BlobCount: res.BlobCount}, false)
}

// clearCrash runs when a snapshot crashed after writing blobs: it re-hashes
// every blob that survived the sweep and quarantines the ones whose bytes
// do not match their name, then removes the marker. The Bloom mark alone
// cannot do this: a false positive keeps an orphan, and an orphan that was
// never synced can be garbage of the right size that a later dedup would
// trust. No snapshot can be writing: GC holds the store lock exclusive.
func (s *Store) clearCrash(res *GCResult) error {
	marker, err := s.root.Lstat(dirtyName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("tree backups: %w", err)
	}
	// A blob older than the marker was synced by a committed snapshot
	// before the crashed one began; only newer ones can be garbage. The
	// slack covers coarse filesystem timestamps.
	since := marker.ModTime().Add(-2 * time.Second)
	var bad []string
	err = s.walkBlobs(func(sum string, size int64) error {
		fi, err := s.root.Lstat(blobName(sum))
		if err != nil {
			return err
		}
		if fi.ModTime().Before(since) {
			return nil
		}
		got, err := s.hashFile(blobName(sum))
		if err != nil {
			return err
		}
		if got != sum {
			bad = append(bad, sum)
		}
		return nil
	}, nil)
	if err != nil {
		return fmt.Errorf("tree backups: re-hashing after a crash: %w", err)
	}
	for _, sum := range bad {
		if err := s.quarantine(sum); err != nil {
			return err
		}
		res.Quarantined = append(res.Quarantined, sum)
	}
	if err := s.root.Remove(dirtyName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("tree backups: %w", err)
	}
	return nil
}

// markManifest streams one manifest into bf, calling each (when set) for
// every referenced blob.
func (s *Store) markManifest(id string, bf *bloom, each func(e *Entry)) error {
	rd, err := s.Open(id)()
	if err != nil {
		return err
	}
	defer rd.Close()
	for {
		e, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if !referencesBlob(e) {
			continue
		}
		bf.add(e.SHA256)
		if each != nil {
			each(e)
		}
	}
}

// referencesBlob reports whether e's content is stored: a secret is hashed
// but never stored.
func referencesBlob(e *Entry) bool { return e.SHA256 != "" && e.State != StateSecret }

// walkBlobs calls blob for every blob file, and stray (when set) for every
// other name under blobs/sha256. Only a 62-hex regular file in a 2-hex
// directory is a blob; nothing else is ever passed to blob.
func (s *Store) walkBlobs(blob func(sum string, size int64) error, stray func(name string)) error {
	top, err := s.root.Open(sumsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	fans, err := top.ReadDir(-1)
	top.Close()
	if err != nil {
		return err
	}
	for _, fan := range fans {
		if len(fan.Name()) != 2 || !isHex(fan.Name()) || !fan.IsDir() {
			if stray != nil {
				stray(sumsDir + "/" + fan.Name())
			}
			continue
		}
		if err := s.walkFan(fan.Name(), blob, stray); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) walkFan(fan string, blob func(sum string, size int64) error, stray func(name string)) error {
	d, err := s.root.Open(sumsDir + "/" + fan)
	if err != nil {
		return err
	}
	defer d.Close()
	for {
		ents, err := d.ReadDir(4096)
		for _, de := range ents {
			name := de.Name()
			if len(name) != 62 || !isHex(name) || !de.Type().IsRegular() {
				if stray != nil {
					stray(sumsDir + "/" + fan + "/" + name)
				}
				continue
			}
			fi, ierr := de.Info()
			if errors.Is(ierr, fs.ErrNotExist) {
				continue
			}
			if ierr != nil {
				return ierr
			}
			if berr := blob(fan+name, fi.Size()); berr != nil {
				return berr
			}
		}
		if errors.Is(err, io.EOF) || (err == nil && len(ents) == 0) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// KeepSet returns the ids retention keeps. Manifests whose header is
// invalid are always kept.
func KeepSet(infos []ManifestInfo, k Keep) map[string]bool {
	keep := make(map[string]bool)
	var valid []ManifestInfo
	for _, in := range infos {
		if in.Header == nil {
			keep[in.ID] = true
			continue
		}
		valid = append(valid, in)
	}
	slices.SortFunc(valid, func(a, b ManifestInfo) int { return cmpUint(b.Header.Seq, a.Header.Seq) })

	for i := 0; i < k.Last && i < len(valid); i++ {
		keep[valid[i].ID] = true
	}
	newestPer := func(n int, period func(time.Time) int) {
		if n <= 0 {
			return
		}
		newest := map[int]string{}
		for _, in := range valid {
			p := period(in.Header.Created.UTC())
			if _, ok := newest[p]; !ok {
				newest[p] = in.ID
			}
		}
		periods := make([]int, 0, len(newest))
		for p := range newest {
			periods = append(periods, p)
		}
		slices.Sort(periods)
		slices.Reverse(periods)
		for i := 0; i < n && i < len(periods); i++ {
			keep[newest[periods[i]]] = true
		}
	}
	newestPer(k.Daily, func(t time.Time) int { return t.Year()*1000 + t.YearDay() })
	newestPer(k.Weekly, func(t time.Time) int {
		y, w := t.ISOWeek()
		return y*100 + w
	})
	complete, vmOff := false, false
	for _, in := range valid {
		h := in.Header
		if h.Complete && !complete {
			keep[in.ID], complete = true, true
		}
		if h.Quiescent == QuiescentVMOff && !vmOff {
			keep[in.ID], vmOff = true, true
		}
		if h.Pinned {
			keep[in.ID] = true
		}
	}
	return keep
}

// bloom is a Bloom filter over blob digests. A false positive only keeps a
// garbage blob until a later GC, whose fresh seeds make a different set of
// false positives.
type bloom struct {
	bits   []uint64
	m      uint64
	k      uint64
	s1, s2 maphash.Seed
}

// newBloom sizes the filter for n digests at a 1% false-positive rate.
func newBloom(n int64) *bloom {
	n = max(n, 1)
	m := uint64(math.Ceil(-float64(n) * math.Log(0.01) / (math.Ln2 * math.Ln2)))
	m = max(m, 64)
	return &bloom{
		bits: make([]uint64, (m+63)/64),
		m:    m,
		k:    7,
		s1:   maphash.MakeSeed(),
		s2:   maphash.MakeSeed(),
	}
}

func (b *bloom) positions(sum string, f func(i uint64) bool) bool {
	h1 := maphash.String(b.s1, sum)
	h2 := maphash.String(b.s2, sum) | 1
	for i := uint64(0); i < b.k; i++ {
		if !f((h1 + i*h2) % b.m) {
			return false
		}
	}
	return true
}

func (b *bloom) add(sum string) {
	b.positions(sum, func(i uint64) bool {
		b.bits[i/64] |= 1 << (i % 64)
		return true
	})
}

func (b *bloom) has(sum string) bool {
	return b.positions(sum, func(i uint64) bool { return b.bits[i/64]&(1<<(i%64)) != 0 })
}

// errNotRegular reports a store path that should be a regular file.
var errNotRegular = errors.New("not a regular file")

func lstatRegular(r *os.Root, name string) (fs.FileInfo, error) {
	fi, err := r.Lstat(name)
	if err == nil && !fi.Mode().IsRegular() {
		err = errNotRegular
	}
	return fi, err
}
