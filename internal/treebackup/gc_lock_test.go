//go:build linux || darwin

package treebackup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
