package host

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/cli/clitest"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/treebackup"
)

// vmStub reports the machine stopped as the real backends do.
type vmStub struct {
	*stubBackend
	off bool
}

func (v *vmStub) MachineStopped(context.Context) (bool, error) { return v.off, nil }

func treeInstance(t *testing.T, extra string) string {
	t.Helper()
	dir := writeInstance(t, "tree_backup:\n  enabled: true\n"+extra)
	if err := os.MkdirAll(filepath.Join(dir, "workspace", "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workspace", "docs", "a.md"), []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Outside the clock window in which a fresh directory reads as
	// listing-unstable.
	time.Sleep(50 * time.Millisecond)
	t.Chdir(dir)
	prev := startTreeGC
	startTreeGC = func(*cobra.Command, *treeEnv) {}
	t.Cleanup(func() { startTreeGC = prev })
	return dir
}

func vmRoot(off bool) (*cobra.Command, *vmStub) {
	v := &vmStub{stubBackend: &stubBackend{resolveRunUserErr: errors.New("machine not up")}, off: off}
	return newRootWith(func(string, string) (backend.Backend, error) { return v, nil }), v
}

func runTree(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root, _ := vmRoot(true)
	return clitest.Exec(t, root, append([]string{"tree"}, args...)...)
}

func TestTreeCommandsWired(t *testing.T) {
	wantSubcommands(t, "tree", "snapshot", "list", "ls", "log", "diff", "cat", "restore", "verify", "delete", "gc", "lock", "path")
}

func TestTreeSnapshotDiffRestoreFlow(t *testing.T) {
	dir := treeInstance(t, "")
	out, err := runTree(t, "snapshot")
	if err != nil || !strings.Contains(out, ": complete") || !strings.Contains(out, "vm-off") {
		t.Fatalf("snapshot: %v\n%s", err, out)
	}
	if err := os.Remove(filepath.Join(dir, "workspace", "docs", "a.md")); err != nil {
		t.Fatal(err)
	}
	// A directory changed within the clock window before its listing reads
	// as listing-unstable, which would turn the deletion into "?".
	time.Sleep(50 * time.Millisecond)
	if out, err := runTree(t, "snapshot", "-q"); err != nil || out != "" {
		t.Fatalf("snapshot -q: %v %q", err, out)
	}
	if out, err := runTree(t, "list"); err != nil || strings.Count(out, "manual") != 2 {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if out, err := runTree(t, "diff", "latest~1"); err != nil || !strings.Contains(out, "D docs/a.md") {
		t.Fatalf("diff: %v\n%s", err, out)
	}
	if out, err := runTree(t, "log", "--", "docs/a.md"); err != nil || !strings.Contains(out, "  D  absent") {
		t.Fatalf("log -- path: %v\n%s", err, out)
	}
	if out, err := runTree(t, "ls", "latest~1", "docs"); err != nil || !strings.Contains(out, "docs/a.md") {
		t.Fatalf("ls: %v\n%s", err, out)
	}
	if out, err := runTree(t, "cat", "latest~1", "q:docs/a.md"); err != nil || out != "alpha\nbeta\n" {
		t.Fatalf("cat: %v %q", err, out)
	}
	target := filepath.Join(t.TempDir(), "r")
	if out, err := runTree(t, "restore", "latest~1", "--to", target); err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(filepath.Join(target, "docs", "a.md")); err != nil || string(b) != "alpha\nbeta\n" {
		t.Fatalf("restored: %q %v", b, err)
	}
	inTree := filepath.Join(dir, "workspace", "restored")
	if _, err := runTree(t, "restore", "latest~1", "--to", inTree); err == nil {
		t.Fatal("a restore into the tree was accepted")
	}
	if out, err := runTree(t, "verify"); err != nil {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	if out, err := runTree(t, "gc"); err != nil {
		t.Fatalf("gc: %v\n%s", err, out)
	}
	if out, err := runTree(t, "delete", "--yes", "latest~1"); err != nil || !strings.Contains(out, "deleted snapshot") {
		t.Fatalf("delete: %v\n%s", err, out)
	}
	if out, err := runTree(t, "path"); err != nil || strings.TrimSpace(out) != filepath.Join(dir, ".lever-state", treebackup.StoreDirName) {
		t.Fatalf("path: %v %q", err, out)
	}
}

func TestTreeSnapshotQuiescenceFollowsTheMachine(t *testing.T) {
	treeInstance(t, "")
	root, _ := vmRoot(false)
	if out, err := clitest.Exec(t, root, "tree", "snapshot"); err != nil || !strings.Contains(out, "(manual, none)") {
		t.Fatalf("snapshot with the VM up: %v\n%s", err, out)
	}
}

func TestTreeSnapshotOffRefuses(t *testing.T) {
	dir := writeInstance(t, "")
	t.Chdir(dir)
	if _, err := runTree(t, "snapshot"); err == nil || !strings.Contains(err.Error(), "tree_backup") {
		t.Fatalf("snapshot with tree backups off: %v", err)
	}
}

func TestTreeIncompleteExitsThree(t *testing.T) {
	dir := treeInstance(t, "  limits: {dir_entries: 1}\n")
	if err := os.WriteFile(filepath.Join(dir, "workspace", "docs", "b.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runTree(t, "snapshot")
	var ee *cli.ExitError
	if !errors.As(err, &ee) || ee.Code != exitTreeIncomplete {
		t.Fatalf("an incomplete snapshot: %v", err)
	}
}

func TestTreeWriteCommandsRefuseInsideLock(t *testing.T) {
	treeInstance(t, "")
	t.Setenv(treebackup.EnvStore, "/somewhere")
	for _, args := range [][]string{{"snapshot"}, {"gc"}, {"delete", "--yes", "x"}, {"verify", "--fix"}, {"lock", "--", "true"}} {
		if _, err := runTree(t, args...); err == nil || !strings.Contains(err.Error(), "inside `lever tree lock`") {
			t.Errorf("%v inside lock: %v", args, err)
		}
	}
}

func TestTreeLockRunsTheCommandWithTheStore(t *testing.T) {
	dir := treeInstance(t, "")
	if _, err := runTree(t, "snapshot"); err != nil {
		t.Fatal(err)
	}
	out, err := runTree(t, "lock", "--", "/bin/sh", "-c", `echo "store=$LEVER_TREE_BACKUP_STORE latest=$LEVER_TREE_BACKUP_LATEST"; exit 7`)
	var ee *cli.ExitError
	if !errors.As(err, &ee) || ee.Code != 7 {
		t.Fatalf("lock: exit %v", err)
	}
	if !strings.Contains(out, "store="+filepath.Join(dir, ".lever-state", treebackup.StoreDirName)+" latest=2") {
		t.Fatalf("lock env:\n%s", out)
	}
}

// A shell the operator runs under `lever tree lock` finds its programs
// through PATH: a tree directory on it would run an agent's program.
func TestTreeLockFiltersPath(t *testing.T) {
	tree := t.TempDir()
	planted := filepath.Join(tree, "bin")
	if err := os.Mkdir(planted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planted, "restic"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(planted, link); err != nil {
		t.Fatal(err)
	}
	good := t.TempDir()
	if err := os.WriteFile(filepath.Join(good, "restic"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pathEnv := strings.Join([]string{"", "relative/bin", planted, link, good}, string(filepath.ListSeparator))
	filtered, bin, err := lockCommandPath("restic", pathEnv, tree)
	if err != nil {
		t.Fatal(err)
	}
	if filtered != good {
		t.Fatalf("filtered PATH = %q, want only %q", filtered, good)
	}
	if want, _ := filepath.EvalSymlinks(filepath.Join(good, "restic")); bin != want {
		t.Fatalf("command = %q", bin)
	}
	if _, _, err := lockCommandPath(filepath.Join(link, "restic"), pathEnv, tree); err == nil {
		t.Fatal("a command inside the tree (through a link) was accepted")
	}
	if _, _, err := lockCommandPath("restic", planted, tree); err == nil {
		t.Fatal("a command found only inside the tree was accepted")
	}
}

func TestStopTakesTreeSnapshotAfterPowerOff(t *testing.T) {
	treeInstance(t, "")
	root, v := vmRoot(true)
	out, err := clitest.Exec(t, root, "stop")
	if err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	if !v.stopped || !strings.Contains(out, "tree snapshot") || !strings.Contains(out, "(stop, vm-off)") {
		t.Fatalf("stop output:\n%s", out)
	}
	root, _ = vmRoot(true)
	if out, err := clitest.Exec(t, root, "stop", "--no-tree-backup"); err != nil || strings.Contains(out, "tree snapshot") {
		t.Fatalf("stop --no-tree-backup: %v\n%s", err, out)
	}
	root, _ = vmRoot(true)
	if out, err := clitest.Exec(t, root, "destroy"); err != nil || !strings.Contains(out, "(destroy, vm-off)") {
		t.Fatalf("destroy: %v\n%s", err, out)
	}
}

func TestStopTreeSnapshotFailureOnlyWarns(t *testing.T) {
	dir := treeInstance(t, "")
	// An unsafe store: the snapshot fails, the stop does not.
	store := filepath.Join(dir, ".lever-state", treebackup.StoreDirName)
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store, 0o755); err != nil {
		t.Fatal(err)
	}
	root, v := vmRoot(true)
	out, err := clitest.Exec(t, root, "stop")
	if err != nil || !v.stopped || !strings.Contains(out, "warning: tree snapshot failed") {
		t.Fatalf("stop: %v\n%s", err, out)
	}
}

func TestStopAtStopFalseTakesNoSnapshot(t *testing.T) {
	treeInstance(t, "  at_stop: false\n")
	root, _ := vmRoot(true)
	if out, err := clitest.Exec(t, root, "stop"); err != nil || strings.Contains(out, "tree snapshot") {
		t.Fatalf("stop with at_stop false: %v\n%s", err, out)
	}
}

func TestDoctorTreeBackupRow(t *testing.T) {
	dir := treeInstance(t, "")
	app, err := config.Load(filepath.Join(dir, config.CanonicalName))
	if err != nil {
		t.Fatal(err)
	}
	st := stateFor(filepath.Join(dir, "lever.yaml"))
	now := time.Now()

	if r := checkTreeBackup(app, st, now); !r.ok || !strings.Contains(r.detail, "no snapshot yet") {
		t.Fatalf("before any snapshot: %+v", r)
	}
	if _, err := runTree(t, "snapshot"); err != nil {
		t.Fatal(err)
	}
	if r := checkTreeBackup(app, st, now); !r.ok || r.fix != "" || !strings.Contains(r.detail, "(manual, vm-off, complete)") {
		t.Fatalf("after a complete snapshot: %+v", r)
	}
	if r := checkTreeBackup(app, st, now.Add(8*24*time.Hour)); !r.ok || !strings.Contains(r.detail, "no snapshot in 7 days") {
		t.Fatalf("a week later: %+v", r)
	}
	store := filepath.Join(st.Dir, treebackup.StoreDirName)
	if err := os.Remove(filepath.Join(store, "reserve")); err != nil {
		t.Fatal(err)
	}
	if r := checkTreeBackup(app, st, now); !r.ok || !strings.Contains(r.detail, "no space is reserved") {
		t.Fatalf("no reserve: %+v", r)
	}
	s, err := treebackup.OpenStore(store, false)
	if err != nil {
		t.Fatal(err)
	}
	err = s.WriteGCStatus(treebackup.GCStatus{Time: now, Result: treebackup.GCOK, LastOK: now, Evicted: 2, LastEvicted: now})
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	if r := checkTreeBackup(app, st, now); !r.ok || !strings.Contains(r.detail, "under max_store") {
		t.Fatalf("an eviction: %+v", r)
	}
	if err := os.WriteFile(filepath.Join(store, "snapshots", ".DS_Store"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := checkTreeBackup(app, st, now); !r.ok || !strings.Contains(r.detail, "stray files") {
		t.Fatalf("a stray file: %+v", r)
	}
	if err := os.Chmod(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if r := checkTreeBackup(app, st, now); r.ok {
		t.Fatalf("an unsafe store: %+v", r)
	}
	app.TreeBackup.Enabled = false
	if r := checkTreeBackup(app, st, now); !r.ok || !strings.Contains(r.detail, "off") {
		t.Fatalf("off: %+v", r)
	}
}

// floodRunner stands in for a hostile guest: whatever it is asked to run,
// it runs `yes`, which writes until it is killed.
type floodRunner struct{ proc.RealRunner }

func (f floodRunner) Run(ctx context.Context, env map[string]string, _ string, _ ...string) (proc.Result, error) {
	return f.RealRunner.Run(ctx, env, "yes")
}

// The keep-vm sync is a guest exec: its output is capped like every other
// one, and a guest that floods it fails the sync at once.
func TestGuestSyncOutputIsCapped(t *testing.T) {
	if _, err := exec.LookPath("yes"); err != nil {
		t.Skip("no yes(1)")
	}
	b := &stubBackend{runner: floodRunner{}}
	start := time.Now()
	err := guestSync(context.Background(), b)
	if !errors.Is(err, proc.ErrOutputLimit) {
		t.Fatalf("guestSync = %v, want the output limit", err)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("guestSync took %v", d)
	}
	if q := quiescentKeepVM(true, err)(context.Background()); q != treebackup.QuiescentNone {
		t.Fatalf("a flooded sync is labelled %s", q)
	}
}
