//go:build linux || darwin

package treebackup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const helperTreeEnv, helperStateEnv = "LEVER_TB_HELPER_TREE", "LEVER_TB_HELPER_STATE"

// TestHelperSnapshotProcess is a second process that takes one snapshot
// with a short lock wait, as lever stop does with lock_wait: the store's
// locks are per process, so a GC and a snapshot in one test need two.
func TestHelperSnapshotProcess(t *testing.T) {
	tree, state := os.Getenv(helperTreeEnv), os.Getenv(helperStateEnv)
	if tree == "" {
		t.Skip("helper")
	}
	o := snapEnv{tree: tree, state: state}.opts(TriggerStop)
	o.LockWait = 300 * time.Millisecond
	res, err := Snapshot(context.Background(), o)
	if err != nil {
		fmt.Println("SNAPSHOT-ERR", err)
		return
	}
	fmt.Println("SNAPSHOT-OK", res.Header.ID)
}

// A GC that takes long over its manifests must not make a stop's snapshot
// skip: the snapshot runs while GC validates and marks, and GC keeps what
// that snapshot committed.
func TestSnapshotRunsWhileGCValidates(t *testing.T) {
	e := newSnapEnv(t)
	writeFile(t, filepath.Join(e.tree, "a"), "one\n")
	e.snap(t)
	writeFile(t, filepath.Join(e.tree, "a"), "two\n")
	var out []byte
	gcPhaseHook = func(stage string) {
		if stage != "validated" || out != nil {
			return
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperSnapshotProcess$", "-test.v", "-test.count=1")
		cmd.Env = append(os.Environ(), helperTreeEnv+"="+e.tree, helperStateEnv+"="+e.state)
		out, _ = cmd.CombinedOutput()
	}
	t.Cleanup(func() { gcPhaseHook = nil })
	s := e.store(t)
	res, err := s.GC(GCOptions{Keep: Keep{Last: 5}, Wait: time.Second})
	if err != nil || res.Busy {
		t.Fatalf("GC: %v %+v", err, res)
	}
	if !strings.Contains(string(out), "SNAPSHOT-OK") {
		t.Fatalf("the snapshot during GC did not commit:\n%s", out)
	}
	infos, _, _ := s.List()
	if len(infos) != 2 {
		t.Fatalf("%d snapshots after GC", len(infos))
	}
	rep, err := s.Verify(false, time.Second)
	if err != nil || len(rep.MissingBlobs) != 0 || len(rep.BadBlobs) != 0 {
		t.Fatalf("verify after GC: %v %+v", err, rep)
	}
}

// Past max_store, GC deletes the oldest snapshots that are not protected:
// the newest, the newest complete, the newest vm-off and pinned ones stay,
// even when retention would keep the others.
func TestGCEvictsOldestUnprotectedPastMaxStore(t *testing.T) {
	e := newSnapEnv(t)
	var ids []string
	for i := 1; i <= 6; i++ {
		os.RemoveAll(filepath.Join(e.tree, "data"))
		writeFile(t, filepath.Join(e.tree, "data", fmt.Sprintf("f%d", i)), strings.Repeat(string(rune('a'+i)), 256<<10))
		o := e.opts(TriggerManual)
		o.Pinned = i == 2
		o.Now = func() time.Time { return time.Date(2026, 10, 1, i, 0, 0, 0, time.UTC) }
		res, err := Snapshot(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.Header.ID)
	}
	s := e.store(t)
	res, err := s.GC(GCOptions{Keep: Keep{Last: 20, Daily: 7, Weekly: 8}, MaxStore: 700 << 10})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{ids[0], ids[2], ids[3], ids[4]}
	if !slices.Equal(res.Evicted, want) || !slices.Equal(res.Deleted, want) {
		t.Fatalf("evicted %v, deleted %v; want %v", res.Evicted, res.Deleted, want)
	}
	infos, _, _ := s.List()
	if len(infos) != 2 || infos[0].ID != ids[1] || infos[1].ID != ids[5] {
		t.Fatalf("left %+v", infos)
	}
	if res.BlobCount != 2 || res.StoreBytes > 700<<10 {
		t.Fatalf("store after the eviction: %d blobs, %d bytes", res.BlobCount, res.StoreBytes)
	}
	st, err := s.ReadGCStatus()
	if err != nil || st.Evicted != 4 || st.LastEvicted.IsZero() {
		t.Fatalf("status %+v, %v", st, err)
	}
	// Under the cap nothing more goes.
	if res, err := s.GC(GCOptions{Keep: Keep{Last: 20}, MaxStore: 700 << 10}); err != nil || len(res.Deleted) != 0 {
		t.Fatalf("second GC: %v %+v", err, res)
	}
	if st, _ := s.ReadGCStatus(); st.Evicted != 0 || st.LastEvicted.IsZero() {
		t.Fatalf("status after a GC that evicted nothing: %+v", st)
	}
}
