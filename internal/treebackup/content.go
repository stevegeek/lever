package treebackup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
)

const (
	readChunk     = 1 << 20
	spaceInterval = 64 << 20
	readAttempts  = 3
)

type readResult struct {
	state  State
	reason Reason
	sum    string
	// size, times, mode and nlink come from the final fstat of the fd read,
	// so they describe the object whose bytes were stored.
	size  int64
	mtime int64
	ctime int64
	mode  uint32
	nlink uint64
	// opened: the recorded object was opened, so another hard-linked path
	// would not do better.
	opened bool
}

// budgets are what a read may still spend: bytes read from the tree, and
// disk that new blobs may take in the store.
type budgets struct{ read, new int64 }

// pass2Content gives each writer root an equal part of the read and new
// budgets left after class 0, so a root's unused share flows on to the
// later ones. The new budget applies once any snapshot of this tree exists:
// the first one stores the whole tree, within the read budget. Waiting for
// a complete one would let an agent that keeps every snapshot incomplete
// escape it.
func (w *walker) pass2Content() error {
	w.pass2 = true
	class0, class1, reverify := w.selectContent()
	if len(class0)+len(class1)+len(reverify) == 0 {
		return nil
	}
	w.readBuf = make([]byte, readChunk)
	b := budgets{read: max(w.o.Limits.Read, 0), new: NoNewLimit}
	if w.o.Known != nil || w.o.Cache != nil {
		b.new = max(w.o.Limits.New, 0)
	}
	for _, g := range class0 {
		if err := w.readGroup(g, &b, false); err != nil {
			return err
		}
	}
	for i, r := range w.roots {
		left := int64(len(w.roots) - i)
		share := budgets{read: b.read / left, new: b.new / left}
		start := share
		for _, g := range class1[r] {
			if err := w.readGroup(g, &share, false); err != nil {
				return err
			}
		}
		b.read -= start.read - share.read
		b.new -= start.new - share.new
	}
	for _, g := range reverify {
		if err := w.readGroup(g, &b, true); err != nil {
			return err
		}
	}
	return nil
}

// readGroup: a re-verification never sets a state; it only replaces a hash
// the stat cache got wrong, or advances verified.
func (w *walker) readGroup(g *group, b *budgets, reverify bool) error {
	skip := func(r Reason) {
		if reverify {
			return
		}
		for _, n := range g.members {
			n.e.State, n.e.Reason = StateSkipped, r
		}
	}
	if reason, err := w.halted(); err != nil {
		return err
	} else if reason != "" {
		skip(reason)
		return nil
	}
	if w.diskLow && !g.secret {
		skip(ReasonDisk)
		return nil
	}
	if g.size >= b.read {
		skip(ReasonRead)
		return nil
	}
	var res readResult
	i := 0
	for ; i < len(g.members); i++ {
		var err error
		res, err = w.readOne(g.members[i], b, g.secret)
		if err != nil {
			return err
		}
		if res.opened || i == len(g.members)-1 {
			break
		}
		if !reverify {
			w.apply(g.members[i], res, g.secret)
		}
	}
	for _, n := range g.members[i:] {
		if reverify {
			if res.sum != "" && res.state == StateOK {
				n.e.SHA256, n.e.Verified = res.sum, w.t.Unix()
				n.e.Size, n.e.Mtime, n.e.Ctime = res.size, res.mtime, res.ctime
				n.e.Mode, n.e.Nlink = res.mode, res.nlink
			}
			continue
		}
		w.apply(n, res, g.secret)
	}
	return nil
}

func (w *walker) apply(n *node, res readResult, secret bool) {
	if res.sum == "" {
		n.e.State, n.e.Reason = res.state, res.reason
		return
	}
	n.e.SHA256, n.e.Verified = res.sum, w.t.Unix()
	n.e.Size, n.e.Mtime, n.e.Ctime = res.size, res.mtime, res.ctime
	n.e.Mode, n.e.Nlink = res.mode, res.nlink
	n.e.State, n.e.Reason = res.state, ""
	if secret {
		n.e.State = StateSecret
	}
}

type attempt struct {
	bw     BlobWriter
	h      hash.Hash
	n      int64
	stable bool
	after  sysStat
}

func (a *attempt) abort() {
	if a != nil && a.bw != nil {
		a.bw.Abort()
		a.bw = nil
	}
}

// readOne: reads are bounded by st_size, never by EOF, so a file that never ends
// costs nothing; a file changing during the copy is retried and, failing
// that, kept as read and marked unstable.
func (w *walker) readOne(n *node, b *budgets, secret bool) (readResult, error) {
	rel := n.e.Path
	dfd, ok := w.dirs.get(ParentOf(rel))
	if !ok {
		return readResult{state: StateChanged}, nil
	}
	if walkSeams.beforeOpen != nil {
		walkSeams.beforeOpen(rel)
	}
	fd, err := sysOpenFile(dfd, string(BaseName(rel)))
	if err != nil {
		switch classify(err) {
		case errCrossDev:
			return readResult{state: StateMount}, nil
		case errDenied:
			return readResult{state: StateUnreadable}, nil
		default:
			return readResult{state: StateChanged}, nil
		}
	}
	defer sysClose(fd)
	var res readResult
	fst, err := sysFstat(fd)
	if err != nil || fst.Mode&modeFmt != modeReg || fst.id() != (DevIno{n.e.Dev, n.e.Ino}) {
		res.state = StateChanged
		return res, nil
	}
	res.opened = true
	switch {
	case platformChecksDev && fst.Dev != w.dev0:
		res.state = StateMount
		return res, nil
	case fst.Dataless:
		res.state = StateDataless
		return res, nil
	}

	var last *attempt
	for try := 1; try <= readAttempts; try++ {
		if fst.Size > w.o.Limits.File {
			last.abort()
			res.state = StateTooBig
			return res, nil
		}
		// One byte more than st_size: the probe for content past the end.
		if fst.Size >= b.read {
			if last != nil {
				break
			}
			res.state, res.reason = StateSkipped, ReasonRead
			return res, nil
		}
		a, stop, err := w.copyOnce(fd, rel, fst, &b.read, secret, try)
		if err != nil {
			last.abort()
			return res, err
		}
		if stop.state != StateOK {
			last.abort()
			stop.opened = true
			return stop, nil
		}
		last.abort()
		last = a
		if a.stable {
			break
		}
		fst = a.after
	}

	res.size, res.mtime, res.ctime = last.n, last.after.Mtime, last.after.Ctime
	res.mode, res.nlink = last.after.Mode&0o7777, last.after.Nlink
	if !last.stable {
		res.state = StateUnstable
	}
	if secret {
		res.sum = hex.EncodeToString(last.h.Sum(nil))
		return res, nil
	}
	br, err := last.bw.Commit(b.new)
	last.bw = nil
	if err != nil {
		if errors.Is(err, ErrNewBudget) {
			return readResult{state: StateSkipped, reason: ReasonNew, opened: true}, nil
		}
		if errors.Is(err, ErrDiskLow) || isNoSpace(err) {
			w.diskLow = true
			return readResult{state: StateSkipped, reason: ReasonDisk, opened: true}, nil
		}
		return res, fmt.Errorf("tree backups: store a blob: %w", err)
	}
	if br.New {
		w.blobs.New++
		w.bytes.New += br.Size
	}
	b.new -= br.Stored
	if br.Repaired {
		w.blobs.Repaired++
	}
	res.sum, res.size = br.Sum, br.Size
	return res, nil
}

// copyOnce: stop carries a state when the copy cannot finish (deadline,
// interrupt, disk, read error); err is a store failure the walk cannot
// record.
func (w *walker) copyOnce(fd int, rel []byte, fst sysStat, budget *int64, secret bool, try int) (*attempt, readResult, error) {
	a := &attempt{}
	diskStop := func(err error) (*attempt, readResult, error) {
		a.abort()
		if errors.Is(err, ErrDiskLow) || isNoSpace(err) {
			w.diskLow = true
			return nil, readResult{state: StateSkipped, reason: ReasonDisk}, nil
		}
		return nil, readResult{}, fmt.Errorf("tree backups: store a blob: %w", err)
	}
	if secret {
		a.h = sha256.New()
	} else {
		if err := w.sink.CheckSpace(); err != nil {
			return diskStop(err)
		}
		bw, err := w.sink.NewBlob()
		if err != nil {
			return diskStop(err)
		}
		a.bw = bw
	}
	var off, sinceCheck int64
	short := false
	for off < fst.Size {
		if reason, err := w.halted(); err != nil {
			a.abort()
			return nil, readResult{}, err
		} else if reason != "" {
			a.abort()
			return nil, readResult{state: StateSkipped, reason: reason}, nil
		}
		want := min(int64(len(w.readBuf)), fst.Size-off)
		k, err := sysPread(fd, w.readBuf[:want], off)
		if err != nil {
			a.abort()
			return nil, readResult{state: StateChanged}, nil
		}
		if k == 0 {
			short = true
			break
		}
		*budget -= int64(k)
		w.bytes.Read += int64(k)
		if a.h != nil {
			a.h.Write(w.readBuf[:k])
		} else if _, err := a.bw.Write(w.readBuf[:k]); err != nil {
			return diskStop(err)
		}
		off += int64(k)
		a.n = off
		if walkSeams.afterChunk != nil {
			walkSeams.afterChunk(rel, try, off)
		}
		if sinceCheck += int64(k); a.bw != nil && sinceCheck >= spaceInterval {
			sinceCheck = 0
			if err := w.sink.CheckSpace(); err != nil {
				return diskStop(err)
			}
		}
	}
	grown := false
	if !short {
		k, err := sysPread(fd, w.readBuf[:1], fst.Size)
		if k > 0 {
			*budget -= int64(k)
			w.bytes.Read += int64(k)
		}
		grown = k > 0 || err != nil
	}
	after, err := sysFstat(fd)
	if err != nil {
		a.abort()
		return nil, readResult{state: StateChanged}, nil
	}
	a.after = after
	a.stable = !short && !grown && after.Size == fst.Size && after.Mtime == fst.Mtime && after.Ctime == fst.Ctime
	return a, readResult{}, nil
}
