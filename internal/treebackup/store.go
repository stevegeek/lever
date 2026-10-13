package treebackup

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/stevegeek/lever/internal/hostledger"
)

// Names inside the store.
const (
	lockName         = "lock"
	snapshotLockName = "snapshot.lock"
	seqName          = "seq"
	dirtyName        = "dirty"
	reserveName      = "reserve"
	snapshotsDir     = "snapshots"
	tmpDir           = "tmp"
	blobsDir         = "blobs"
	sumsDir          = "blobs/sha256"
	corruptDir       = "blobs/corrupt"
	manifestExt      = ".jsonl"
)

// minReserve is the smallest reserve kept for the manifest write. It is a
// variable for tests.
var minReserve int64 = 64 << 20

// manifestHeaderRoom is what the header and the trailer can add to the
// entry lines charged to the manifest budget.
const manifestHeaderRoom = 1 << 20

// ManifestSpace is the most manifest the store's filesystem is sure to
// hold at commit time: the reserve's allocated blocks, plus the free space
// that blobs may not take (min_free, or all of it when no blob may be
// written), less what one blob can run past min_free between two checks,
// less the header. ok is false when the free space cannot be read.
func (s *Store) ManifestSpace(minFree int64) (space int64, ok bool) {
	free, _, _, err := statfsFunc(s.dirf)
	if err != nil {
		return 0, false
	}
	var reserve int64
	if fi, err := lstatRegular(s.root, reserveName); err == nil {
		reserve = min(fi.Size(), allocated(fi))
	}
	keep := clampMul(free, 1)
	if minFree >= 0 && minFree < keep {
		keep = minFree
	}
	return max(reserve+max(keep-spaceInterval-readChunk, 0)-manifestHeaderRoom, 0), true
}

// Store is a tree backup store: content-addressed blobs and one manifest
// per snapshot. Only the host user may own or write it, since it holds
// everything the tree held.
type Store struct {
	dir  string
	root *os.Root
	dirf *os.File
	key  string
	// crashed: MarkDirty found the marker of a snapshot that never
	// committed. Its unsynced orphans can be garbage of the right size, so
	// the marker stays until a GC has re-hashed every surviving blob.
	crashed bool
}

// OpenStore opens the store at dir, which must be a real directory, mode
// 0700 and owned by this user (ErrUnsafe otherwise). create makes it and
// its layout when missing.
func OpenStore(dir string, create bool) (*Store, error) {
	if !platformSupported {
		return nil, errPlatform
	}
	if create {
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("tree backups: %w", err)
		}
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("tree backups: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrUnsafe, dir)
	}
	if fi.Mode().Perm() != 0o700 {
		return nil, fmt.Errorf("%w: %s is %v; it must be 0700 (chmod 700 it)", ErrUnsafe, dir, fi.Mode().Perm())
	}
	if owner, ok := hostledger.FileOwner(fi); !ok || owner != os.Getuid() {
		return nil, fmt.Errorf("%w: %s is not owned by this user", ErrUnsafe, dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("tree backups: %w", err)
	}
	s := &Store{dir: dir, root: root}
	s.dirf, err = root.Open(".")
	if err != nil {
		root.Close()
		return nil, fmt.Errorf("tree backups: %w", err)
	}
	// The directory checked by Lstat must be the one the root holds, or a
	// swap between the two would go unnoticed.
	rfi, err := s.dirf.Stat()
	if err == nil && !os.SameFile(fi, rfi) {
		err = fmt.Errorf("%w: %s changed while it was opened", ErrUnsafe, dir)
	}
	if err == nil {
		var ok bool
		if s.key, ok = fileKey(rfi); !ok {
			err = fmt.Errorf("tree backups: cannot identify %s", dir)
		}
	}
	if err == nil && create {
		err = s.ensureLayout()
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

var errPlatform = errors.New("tree backups: unsupported platform")

func (s *Store) ensureLayout() error {
	for _, d := range []string{blobsDir, sumsDir, corruptDir, snapshotsDir, tmpDir} {
		if err := s.root.Mkdir(d, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("tree backups: %w", err)
		}
	}
	return nil
}

// Dir is the store's path.
func (s *Store) Dir() string { return s.dir }

// Close releases the store's descriptors. Locks are released separately.
func (s *Store) Close() error {
	if s.dirf != nil {
		s.dirf.Close()
	}
	return s.root.Close()
}

func randomSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// createTemp makes a new file in tmp/, never over an existing one.
func (s *Store) createTemp(prefix string) (*os.File, string, error) {
	if err := s.root.Mkdir(tmpDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, "", fmt.Errorf("tree backups: %w", err)
	}
	name := tmpDir + "/" + prefix + randomSuffix()
	f, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|oNoFollow, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("tree backups: %w", err)
	}
	return f, name, nil
}

// replaceFile writes b to name through a temp file and a rename. A GC
// that starts between the two clears tmp/; the write is then tried once
// more.
func (s *Store) replaceFile(name string, b []byte) error {
	err := s.replaceFileOnce(name, b)
	if errors.Is(err, fs.ErrNotExist) {
		err = s.replaceFileOnce(name, b)
	}
	if err != nil {
		return fmt.Errorf("tree backups: write %s: %w", name, err)
	}
	return fsync(s.dirf)
}

func (s *Store) replaceFileOnce(name string, b []byte) error {
	f, tmp, err := s.createTemp(name + ".")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = fsync(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = s.root.Rename(tmp, name)
	}
	if err != nil {
		_ = s.root.Remove(tmp)
	}
	return err
}

// readSmall reads a store file of at most limit bytes.
func (s *Store) readSmall(name string, limit int64) ([]byte, error) {
	f, err := s.root.OpenFile(name, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("tree backups: %s is larger than %d bytes", name, limit)
	}
	return b, nil
}

func blobName(sum string) string { return sumsDir + "/" + sum[:2] + "/" + sum[2:] }

// OpenBlob opens the blob with that digest. Only a regular file at its
// place in blobs/sha256 counts.
func (s *Store) OpenBlob(sum string) (io.ReadCloser, int64, error) {
	if !validSum(sum) {
		return nil, 0, fmt.Errorf("tree backups: bad blob digest %q", sum)
	}
	f, err := s.root.OpenFile(blobName(sum), os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("tree backups: blob %s: %w", sum, err)
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = errors.New("not a regular file")
	}
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("tree backups: blob %s: %w", sum, err)
	}
	return f, fi.Size(), nil
}

// MarkDirty creates the dirty marker before the first blob of a snapshot
// is written. wasDirty reports a marker left by a snapshot that never
// committed: its blobs may hold garbage, so dedup hits must be re-hashed.
func (s *Store) MarkDirty() (wasDirty bool, err error) {
	if err := s.ensureLayout(); err != nil {
		return false, err
	}
	fi, err := s.root.Lstat(dirtyName)
	if err == nil {
		if !fi.Mode().IsRegular() {
			return true, fmt.Errorf("tree backups: %s is not a regular file", dirtyName)
		}
		s.crashed = true
		return true, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("tree backups: %w", err)
	}
	s.crashed = false
	// The first snapshot has no reserve from an earlier commit. Failing to
	// make one here only weakens the full-disk case, so it is not an error.
	if _, err := s.root.Lstat(reserveName); errors.Is(err, fs.ErrNotExist) {
		_, _ = s.resizeReserve(minReserve, 0)
	}
	f, err := s.root.OpenFile(dirtyName, os.O_CREATE|os.O_EXCL|os.O_WRONLY|oNoFollow, 0o600)
	if err != nil {
		return false, fmt.Errorf("tree backups: %w", err)
	}
	err = fsync(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = fsync(s.dirf)
	}
	if err != nil {
		return false, fmt.Errorf("tree backups: %w", err)
	}
	return false, nil
}

// NextSeq returns the seq the next snapshot takes: one above both the
// high-water mark and every manifest. It writes nothing, so a full disk
// cannot stop a snapshot here; RecordSeq stores the mark once the manifest
// is committed. The caller holds snapshot.lock.
func (s *Store) NextSeq() (uint64, error) {
	var high uint64
	b, err := s.readSmall(seqName, 32)
	switch {
	case err == nil:
		high, err = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("tree backups: %s is corrupt: %q", seqName, b)
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return 0, fmt.Errorf("tree backups: %w", err)
	}
	infos, _, err := s.List()
	if err != nil {
		return 0, err
	}
	for _, in := range infos {
		if in.Header != nil && in.Header.Seq > high {
			high = in.Header.Seq
		}
	}
	return high + 1, nil
}

// RecordSeq stores seq as the high-water mark, so it is never handed out
// again, even after the snapshot that took it is deleted. Until it is
// written, the committed manifest itself holds the mark.
func (s *Store) RecordSeq(seq uint64) error {
	return s.replaceFile(seqName, []byte(strconv.FormatUint(seq, 10)+"\n"))
}

func manifestName(id string) string { return snapshotsDir + "/" + id + manifestExt }

// FreeID returns the id for a snapshot started at t that no manifest
// uses.
func (s *Store) FreeID(t time.Time) (string, error) {
	base := t.UTC().Format("20060102T150405Z")
	for i := 1; i < 1000; i++ {
		id := base
		if i > 1 {
			id += "-" + strconv.Itoa(i)
		}
		_, err := s.root.Lstat(manifestName(id))
		if errors.Is(err, fs.ErrNotExist) {
			return id, nil
		}
		if err != nil {
			return "", fmt.Errorf("tree backups: %w", err)
		}
	}
	return "", fmt.Errorf("tree backups: no free id for %s", base)
}

// CommitManifest makes the snapshot exist: it flushes the blobs, writes
// and fsyncs the manifest in tmp/, validates what it wrote, and renames it
// into snapshots/. A failed validation is a lever bug and commits nothing.
// Problems after the rename are returned as warnings: the snapshot exists.
func (s *Store) CommitManifest(h *Header, entries []Entry) (warnings []string, err error) {
	if !ValidID(h.ID) {
		return nil, fmt.Errorf("tree backups: bad snapshot id %q", h.ID)
	}
	if err := crashPoint("before-sync"); err != nil {
		return nil, err
	}
	if err := syncStoreFS(s.dirf); err != nil {
		return nil, fmt.Errorf("tree backups: flush blobs: %w", err)
	}
	tmp, size, err := s.writeManifestTemp(h, entries, 0, false)
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		tmp, size, err = s.writeManifestTemp(h, entries, 1, true)
		if err == nil {
			warnings = append(warnings, "the disk was full: the manifest used the store's reserve")
		}
	}
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed && commitCrash == nil {
			_ = s.root.Remove(tmp)
		}
	}()
	if err := s.selfCheck(tmp, size, h.ID); err != nil {
		return nil, fmt.Errorf("tree backups: internal error, please report: %w", err)
	}
	if err := crashPoint("before-rename"); err != nil {
		return nil, err
	}
	final := manifestName(h.ID)
	if _, err := s.root.Lstat(final); err == nil {
		return nil, fmt.Errorf("tree backups: snapshot %s already exists", h.ID)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("tree backups: %w", err)
	}
	if err := s.root.Rename(tmp, final); err != nil {
		return nil, fmt.Errorf("tree backups: %w", err)
	}
	committed = true
	if err := s.syncSubdir(snapshotsDir); err != nil {
		warnings = append(warnings, fmt.Sprintf("the snapshot may not survive a power loss: %v", err))
	}
	if !s.crashed {
		if err := s.root.Remove(dirtyName); err != nil && !errors.Is(err, fs.ErrNotExist) {
			warnings = append(warnings, fmt.Sprintf("could not remove the dirty marker: %v", err))
		}
	}
	if err := s.RecordSeq(h.Seq); err != nil {
		warnings = append(warnings, fmt.Sprintf("could not record the snapshot sequence number: %v", err))
	}
	if kept, err := s.resizeReserve(h.Limits.Manifest, size); err != nil {
		warnings = append(warnings, fmt.Sprintf("could not keep %s of reserve for the next manifest (kept %s): %v",
			humanBytes(reserveSize(h.Limits.Manifest, size)), humanBytes(kept), err))
	}
	return warnings, nil
}

func crashPoint(stage string) error {
	if commitCrash != nil {
		return commitCrash(stage)
	}
	return nil
}

// writeManifestTemp writes the manifest to a new file in tmp/. With
// useReserve the file is the reserve itself, renamed into tmp/ and
// overwritten in place: the agent can fill the disk through the tree, and
// space freed by truncating the reserve could be taken again before the
// write, while the reserve's own allocated blocks cannot.
func (s *Store) writeManifestTemp(h *Header, entries []Entry, attempt int, useReserve bool) (string, int64, error) {
	var f *os.File
	var name string
	var err error
	var reserved int64 = -1
	if useReserve {
		name = tmpDir + "/" + h.ID + "." + randomSuffix()
		if err = s.root.Rename(reserveName, name); err == nil {
			f, err = s.root.OpenFile(name, os.O_WRONLY|oNoFollow, 0)
			if err == nil {
				var fi fs.FileInfo
				if fi, err = f.Stat(); err == nil {
					reserved = fi.Size()
				} else {
					f.Close()
					f = nil
				}
			}
			if err != nil {
				s.giveBackReserve(name, -1)
				return "", 0, fmt.Errorf("tree backups: write manifest: %w", err)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", 0, fmt.Errorf("tree backups: write manifest: %w", err)
		}
	}
	if f == nil {
		f, name, err = s.createTemp(h.ID + ".")
		if err != nil {
			return "", 0, err
		}
	}
	if manifestWriteFault != nil {
		err = manifestWriteFault(attempt)
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	cw := &countWriter{w: bw}
	if err == nil {
		err = WriteManifest(cw, h, entries)
	}
	if err == nil {
		err = bw.Flush()
	}
	if err == nil {
		err = f.Truncate(cw.n)
	}
	if err == nil {
		err = syncManifest(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if reserved >= 0 {
			s.giveBackReserve(name, reserved)
		} else {
			_ = s.root.Remove(name)
		}
		return "", 0, fmt.Errorf("tree backups: write manifest: %w", err)
	}
	return name, cw.n, nil
}

// giveBackReserve makes the file at name the reserve again after a failed
// write into it, at its old size when that is known: a failed commit must
// not cost the next one its reserve. The blocks it held are still its own,
// so restoring the size needs no new space unless the write had already
// shrunk it.
func (s *Store) giveBackReserve(name string, size int64) {
	if size >= 0 {
		if f, err := s.root.OpenFile(name, os.O_RDWR|oNoFollow, 0); err == nil {
			_ = preallocate(f, size)
			f.Close()
		}
	}
	if err := s.root.Rename(name, reserveName); err != nil {
		_ = s.root.Remove(name)
	}
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (s *Store) selfCheck(name string, size int64, id string) error {
	_, err := ValidateAll(func() (EntryReader, error) {
		f, err := s.root.OpenFile(name, os.O_RDONLY|oNoFollow, 0)
		if err != nil {
			return nil, err
		}
		rd, err := NewReader(f, size, id)
		if err != nil {
			f.Close()
		}
		return rd, err
	})
	return err
}

func (s *Store) syncSubdir(name string) error {
	d, err := s.root.Open(name)
	if err != nil {
		return err
	}
	defer d.Close()
	return fsync(d)
}

func reserveSize(limit, newest int64) int64 {
	return min(max(minReserve, 2*newest), limit)
}

// resizeReserve sizes the reserve for the next manifest. On a full disk it
// keeps as much as there is room for, since a smaller reserve still saves
// a smaller snapshot. kept is the reserve's size afterwards.
func (s *Store) resizeReserve(limit, newest int64) (kept int64, err error) {
	f, err := s.root.OpenFile(reserveName, os.O_CREATE|os.O_RDWR|oNoFollow, 0o600)
	if err != nil {
		return 0, err
	}
	defer func() {
		if fi, serr := f.Stat(); serr == nil {
			kept = min(fi.Size(), allocated(fi))
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	want := reserveSize(limit, newest)
	err = preallocate(f, want)
	if err == nil || !isNoSpace(err) {
		return 0, err
	}
	fi, serr := f.Stat()
	free, _, _, ferr := statfsFunc(s.dirf)
	if serr != nil || ferr != nil {
		return 0, err
	}
	if part := (min(fi.Size(), allocated(fi)) + clampMul(free, 1)) &^ (1<<20 - 1); part > fi.Size() && part < want {
		if perr := preallocate(f, part); perr != nil {
			_ = f.Truncate(fi.Size())
		}
	}
	return 0, err
}

func humanBytes(n int64) string {
	if n >= 1<<20 {
		return strconv.FormatInt(n>>20, 10) + " MiB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}

// ManifestInfo is one manifest in snapshots/, with its header validated as
// a header. Header is nil, and Err says why, when even the header line is
// invalid.
type ManifestInfo struct {
	ID     string
	Header *Header
	Err    error
}

// List returns the manifests ordered by seq, those with an invalid header
// last (by id), and the other names in snapshots/ as strays.
func (s *Store) List() (infos []ManifestInfo, strays []string, err error) {
	d, err := s.root.Open(snapshotsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("tree backups: %w", err)
	}
	ents, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("tree backups: %w", err)
	}
	for _, de := range ents {
		name := de.Name()
		id, ok := strings.CutSuffix(name, manifestExt)
		if !ok || !ValidID(id) || !de.Type().IsRegular() {
			strays = append(strays, name)
			continue
		}
		info := ManifestInfo{ID: id}
		info.Header, info.Err = s.readHeader(id)
		infos = append(infos, info)
	}
	slices.SortFunc(infos, func(a, b ManifestInfo) int {
		switch {
		case a.Header != nil && b.Header != nil:
			if c := cmpUint(a.Header.Seq, b.Header.Seq); c != 0 {
				return c
			}
		case a.Header != nil:
			return -1
		case b.Header != nil:
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	slices.Sort(strays)
	return infos, strays, nil
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (s *Store) readHeader(id string) (*Header, error) {
	rd, err := s.Open(id)()
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	return rd.Header(), nil
}

// Open returns an OpenManifest over the manifest with that id.
func (s *Store) Open(id string) OpenManifest {
	return func() (EntryReader, error) {
		if !ValidID(id) {
			return nil, fmt.Errorf("tree backups: bad snapshot id %q", id)
		}
		f, err := s.root.OpenFile(manifestName(id), os.O_RDONLY|oNoFollow, 0)
		if err != nil {
			return nil, fmt.Errorf("tree backups: snapshot %s: %w", id, err)
		}
		fi, err := f.Stat()
		if err == nil && !fi.Mode().IsRegular() {
			err = errors.New("not a regular file")
		}
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("tree backups: snapshot %s: %w", id, err)
		}
		rd, err := NewReader(f, fi.Size(), id)
		if err != nil {
			f.Close()
			return nil, err
		}
		return rd, nil
	}
}

// ErrNoSnapshot means a snapshot reference names no valid manifest.
var ErrNoSnapshot = errors.New("tree backups: no such snapshot")

// Resolve turns an id, "latest" or "latest~N" into an id. latest is the
// valid manifest with the highest seq.
func (s *Store) Resolve(ref string) (string, error) {
	infos, _, err := s.List()
	if err != nil {
		return "", err
	}
	valid := slices.DeleteFunc(infos, func(in ManifestInfo) bool { return in.Header == nil })
	back := -1
	switch {
	case ref == "latest":
		back = 0
	case strings.HasPrefix(ref, "latest~"):
		n, err := strconv.Atoi(ref[len("latest~"):])
		if err != nil || n < 0 || strconv.Itoa(n) != ref[len("latest~"):] {
			return "", fmt.Errorf("tree backups: bad snapshot reference %q", ref)
		}
		back = n
	case ValidID(ref):
		for _, in := range valid {
			if in.ID == ref {
				return ref, nil
			}
		}
		return "", fmt.Errorf("%w: %s", ErrNoSnapshot, ref)
	default:
		return "", fmt.Errorf("tree backups: bad snapshot reference %q", ref)
	}
	if back >= len(valid) {
		return "", fmt.Errorf("%w: %s", ErrNoSnapshot, ref)
	}
	return valid[len(valid)-1-back].ID, nil
}

// SinkOptions configure the BlobSink of one snapshot.
type SinkOptions struct {
	// MinFree is the free space, in bytes, below which no new blob starts.
	MinFree int64
	// Rehash is set when the dirty marker was found at the start of this
	// snapshot: an existing blob of the right size may still hold garbage.
	Rehash bool
}

type sink struct {
	s *Store
	o SinkOptions
}

// Sink returns the BlobSink for one snapshot.
func (s *Store) Sink(o SinkOptions) BlobSink { return &sink{s: s, o: o} }

func (k *sink) HasBlob(sum string, size int64) bool {
	if !validSum(sum) {
		return false
	}
	fi, err := k.s.root.Lstat(blobName(sum))
	return err == nil && fi.Mode().IsRegular() && fi.Size() == size
}

// CheckSpace applies min_free and keeps 1% of the inodes free, except on
// filesystems that report no inode count.
func (k *sink) CheckSpace() error {
	free, ffree, files, err := statfsFunc(k.s.dirf)
	if err != nil {
		return fmt.Errorf("tree backups: free space: %w", err)
	}
	if k.o.MinFree > 0 && free < uint64(k.o.MinFree) {
		return ErrDiskLow
	}
	if files > 0 && ffree < files/100 {
		return ErrDiskLow
	}
	return nil
}

func (k *sink) NewBlob() (BlobWriter, error) {
	f, name, err := k.s.createTemp("blob.")
	if err != nil {
		return nil, err
	}
	return &blobWriter{k: k, f: f, name: name, h: sha256.New()}, nil
}

type blobWriter struct {
	k    *sink
	f    *os.File
	name string
	h    hash.Hash
	n    int64
	// stored counts the bytes written to disk: blocks of zeros are left as
	// holes.
	stored int64
	done   bool
}

// holeBlock is the unit blobWriter leaves unwritten when it is all zeros.
const holeBlock = 4 << 10

var zeroBlock [holeBlock]byte

// Write copies and hashes in the same pass: a separate hash pass would let
// the content change between the hash and the copy. A block of zeros at a
// block boundary is skipped, so a sparse file the agent made for free costs
// the store nothing either; Commit sets the final size.
func (w *blobWriter) Write(p []byte) (int, error) {
	if w.done {
		return 0, errors.New("tree backups: write after commit")
	}
	w.h.Write(p)
	for done := 0; done < len(p); {
		k := min(len(p)-done, holeBlock-int(w.n%holeBlock))
		b := p[done : done+k]
		if k < holeBlock || !bytes.Equal(b, zeroBlock[:]) {
			if _, err := w.f.WriteAt(b, w.n); err != nil {
				return done, err
			}
			w.stored += int64(k)
		}
		done += k
		w.n += int64(k)
	}
	return len(p), nil
}

func (w *blobWriter) Abort() {
	if w.done {
		return
	}
	w.done = true
	w.f.Close()
	_ = w.k.s.root.Remove(w.name)
}

func (w *blobWriter) Commit(maxNew int64) (BlobResult, error) {
	if w.done {
		return BlobResult{}, errors.New("tree backups: blob already finished")
	}
	w.done = true
	s := w.k.s
	sum := hex.EncodeToString(w.h.Sum(nil))
	res := BlobResult{Sum: sum, Size: w.n}
	fail := func(err error) (BlobResult, error) {
		w.f.Close()
		_ = s.root.Remove(w.name)
		return BlobResult{}, fmt.Errorf("tree backups: store blob: %w", err)
	}
	dst := blobName(sum)
	fi, err := s.root.Lstat(dst)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		res.New = true
	case err != nil:
		return fail(err)
	case !fi.Mode().IsRegular():
		return fail(fmt.Errorf("%s is not a regular file", dst))
	case fi.Size() != w.n:
		res.Repaired = true
	case w.k.o.Rehash:
		ok, err := s.blobMatches(sum)
		if err != nil {
			return fail(err)
		}
		if ok {
			w.f.Close()
			_ = s.root.Remove(w.name)
			return res, nil
		}
		res.Repaired = true
	default:
		w.f.Close()
		_ = s.root.Remove(w.name)
		return res, nil
	}
	// Charged as the disk the blob takes at the least: its written blocks
	// plus one for the inode and directory entry, so that many tiny blobs
	// cost what they occupy.
	charge := (w.stored+holeBlock-1)/holeBlock*holeBlock + holeBlock
	if res.New && charge > maxNew {
		w.f.Close()
		_ = s.root.Remove(w.name)
		return BlobResult{}, ErrNewBudget
	}
	res.Stored = charge
	if err := w.f.Truncate(w.n); err != nil {
		return fail(err)
	}
	if err := syncNewBlob(w.f); err != nil {
		return fail(err)
	}
	if err := w.f.Chmod(0o400); err != nil {
		return fail(err)
	}
	if err := w.f.Close(); err != nil {
		_ = s.root.Remove(w.name)
		return BlobResult{}, fmt.Errorf("tree backups: store blob: %w", err)
	}
	if err := s.root.Mkdir(path.Dir(dst), 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		_ = s.root.Remove(w.name)
		return BlobResult{}, fmt.Errorf("tree backups: store blob: %w", err)
	}
	if err := s.root.Rename(w.name, dst); err != nil {
		_ = s.root.Remove(w.name)
		return BlobResult{}, fmt.Errorf("tree backups: store blob: %w", err)
	}
	return res, nil
}

// blobMatches re-hashes a stored blob.
func (s *Store) blobMatches(sum string) (bool, error) {
	got, err := s.hashFile(blobName(sum))
	if err != nil {
		return false, err
	}
	return got == sum, nil
}

func (s *Store) hashFile(name string) (string, error) {
	f, err := s.root.OpenFile(name, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// isHex reports whether b is all lowercase hex digits.
func isHex(b string) bool { return len(b) > 0 && strings.Trim(b, "0123456789abcdef") == "" }
