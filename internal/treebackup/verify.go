package treebackup

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"time"
)

// VerifyReport is what Verify found. Strays are store-relative paths that
// are neither manifests nor blobs; Problems explains each bad manifest.
type VerifyReport struct {
	Manifests    int
	Blobs        int
	BadManifests []string
	BadBlobs     []string
	MissingBlobs []string
	Strays       []string
	DuplicateSeq []uint64
	Quarantined  []string
	Problems     []string
}

// OK reports whether verify found nothing wrong. Quarantined blobs still
// count: their content is gone until a snapshot stores it again.
func (r *VerifyReport) OK() bool {
	return len(r.BadManifests) == 0 && len(r.BadBlobs) == 0 && len(r.MissingBlobs) == 0 &&
		len(r.Strays) == 0 && len(r.DuplicateSeq) == 0 && len(r.Problems) == 0
}

// Verify validates every manifest and re-hashes every blob they reference,
// under the shared lock. With fix, it then releases that lock and takes the
// exclusive one (never a conversion) only to re-hash the bad blobs again
// and move those still bad into blobs/corrupt/.
func (s *Store) Verify(fix bool, wait time.Duration) (*VerifyReport, error) {
	l, err := s.LockShared(wait)
	if err != nil {
		return nil, err
	}
	rep, err := s.verifyShared()
	if rerr := l.Release(); err == nil {
		err = rerr
	}
	if err != nil || !fix || len(rep.BadBlobs) == 0 {
		return rep, err
	}
	x, err := s.LockExclusive(wait)
	if err != nil {
		return rep, err
	}
	defer x.Release()
	for _, sum := range rep.BadBlobs {
		ok, err := s.blobMatches(sum)
		if errors.Is(err, fs.ErrNotExist) || ok {
			continue
		}
		if err != nil {
			return rep, fmt.Errorf("tree backups: blob %s: %w", sum, err)
		}
		if err := s.quarantine(sum); err != nil {
			return rep, err
		}
		rep.Quarantined = append(rep.Quarantined, sum)
	}
	return rep, nil
}

func (s *Store) verifyShared() (*VerifyReport, error) {
	rep := &VerifyReport{}
	if fi, err := s.root.Lstat(snapshotsDir); err != nil || !fi.IsDir() {
		rep.Problems = append(rep.Problems, snapshotsDir+" is missing or not a directory")
	}
	infos, strays, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, n := range strays {
		rep.Strays = append(rep.Strays, snapshotsDir+"/"+n)
	}
	var refs int64
	seqs := map[uint64]int{}
	for _, in := range infos {
		if in.Header != nil {
			refs += in.Header.Counts.ByType[TypeFile]
			seqs[in.Header.Seq]++
		}
	}
	for seq, n := range seqs {
		if n > 1 {
			rep.DuplicateSeq = append(rep.DuplicateSeq, seq)
		}
	}
	slices.Sort(rep.DuplicateSeq)

	bf := newBloom(refs)
	missing := map[string]bool{}
	for _, in := range infos {
		rep.Manifests++
		err := in.Err
		if in.Header != nil {
			err = s.markManifest(in.ID, bf, func(e *Entry) {
				if missing[e.SHA256] {
					return
				}
				if _, err := lstatRegular(s.root, blobName(e.SHA256)); err != nil {
					missing[e.SHA256] = true
				}
			})
		}
		if err != nil {
			rep.BadManifests = append(rep.BadManifests, in.ID)
			rep.Problems = append(rep.Problems, err.Error())
		}
	}
	for sum := range missing {
		rep.MissingBlobs = append(rep.MissingBlobs, sum)
	}
	slices.Sort(rep.MissingBlobs)

	err = s.walkBlobs(func(sum string, _ int64) error {
		if !bf.has(sum) {
			return nil
		}
		rep.Blobs++
		ok, err := s.blobMatches(sum)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("blob %s: %w", sum, err)
		}
		if !ok {
			rep.BadBlobs = append(rep.BadBlobs, sum)
		}
		return nil
	}, func(name string) { rep.Strays = append(rep.Strays, name) })
	if err != nil {
		return rep, fmt.Errorf("tree backups: verify: %w", err)
	}
	return rep, nil
}

// quarantine moves a bad blob out of blobs/sha256, so the next snapshot of
// that content stores it again, and keeps it for manual recovery.
func (s *Store) quarantine(sum string) error {
	if err := s.root.Mkdir(corruptDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("tree backups: %w", err)
	}
	for i := 0; i < 1000; i++ {
		dst := corruptDir + "/" + sum
		if i > 0 {
			dst += "." + strconv.Itoa(i)
		}
		if _, err := s.root.Lstat(dst); !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := s.root.Rename(blobName(sum), dst); err != nil {
			return fmt.Errorf("tree backups: quarantine %s: %w", sum, err)
		}
		return nil
	}
	return fmt.Errorf("tree backups: quarantine %s: no free name", sum)
}
