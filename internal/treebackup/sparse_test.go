//go:build linux || darwin

package treebackup

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// diskUsage sums the blocks the files under dir take.
func diskUsage(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		n += fi.Sys().(*syscall.Stat_t).Blocks * 512
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Sparse files cost the agent nothing in the tree; stored densely, each
// stop added their apparent size to the store. Blobs keep the holes.
func TestSparseFilesCostTheStoreNothing(t *testing.T) {
	e := newSnapEnv(t)
	const files, size = 4, 16 << 20
	blobs := filepath.Join(e.state, StoreDirName, "blobs")
	for round := 0; round < 3; round++ {
		for i := 0; i < files; i++ {
			f, err := os.Create(filepath.Join(e.tree, fmt.Sprintf("s%d", i)))
			if err != nil {
				t.Fatal(err)
			}
			f.Truncate(size)
			f.WriteAt([]byte{byte(round + 1)}, int64(i)*8192+size/2)
			f.Close()
		}
		h := e.snap(t)
		if !h.Complete || h.Bytes.New != files*size {
			t.Fatalf("round %d: complete %v, new %d", round, h.Complete, h.Bytes.New)
		}
		if used := diskUsage(t, blobs); used > int64(round+1)*files*64<<10 {
			t.Fatalf("after %d stops the blobs take %d bytes on disk for %d bytes of sparse files", round+1, used, files*size)
		}
	}
	// The content is intact.
	s := e.store(t)
	rep, err := s.Verify(false, 0)
	if err != nil || len(rep.BadBlobs) != 0 {
		t.Fatalf("verify: %v %+v", err, rep)
	}
}

// Once a complete snapshot exists, a snapshot adds at most limits.new of
// disk to the store; past it, new files are skipped(new) and the snapshot
// is incomplete. Damage to files the last complete snapshot held is read
// and stored first.
func TestNewContentBudgetBoundsStoreGrowth(t *testing.T) {
	e := newSnapEnv(t)
	writeFile(t, filepath.Join(e.tree, "known.txt"), "good\n")
	first := e.snap(t)
	if !first.Complete {
		t.Fatal("first snapshot incomplete")
	}
	blobs := filepath.Join(e.state, StoreDirName, "blobs")
	before := diskUsage(t, blobs)
	const per = 1 << 20
	writeFile(t, filepath.Join(e.tree, "known.txt"), strings.Repeat("D", per))
	for i := 0; i < 4; i++ {
		writeFile(t, filepath.Join(e.tree, fmt.Sprintf("new%d", i)), strings.Repeat(string(rune('a'+i)), per))
	}
	o := e.opts(TriggerStop)
	o.Limits.New = 2*per + per/2
	res, err := Snapshot(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	h := res.Header
	if h.Complete || h.Counts.ByState[StateSkipped] != 3 {
		t.Fatalf("complete %v, skipped %d; partial %+v", h.Complete, h.Counts.ByState[StateSkipped], h.Partial)
	}
	if p := h.Partial[0]; p.State != StateSkipped || p.Reason != ReasonNew {
		t.Fatalf("partial %+v", h.Partial)
	}
	if grew := diskUsage(t, blobs) - before; grew > o.Limits.New+64<<10 {
		t.Fatalf("the store grew %d bytes, the budget is %d", grew, o.Limits.New)
	}
	s := e.store(t)
	var ls bytes.Buffer
	if err := Ls(&ls, s.Open(h.ID), []byte("known.txt"), false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ls.String(), "skipped") {
		t.Fatalf("the damaged known file was not stored first: %s", ls.String())
	}
}
