//go:build linux || darwin

package treebackup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/fsutil"
)

// copyBackCommand is the rsync command the restore warning tells the
// operator to copy back with.
func copyBackCommand(t *testing.T) []string {
	t.Helper()
	m := regexp.MustCompile("`(rsync [^`]+)`").FindStringSubmatch(RestoreWarning)
	if m == nil {
		t.Fatalf("no rsync command in the restore warning: %s", RestoreWarning)
	}
	return strings.Fields(m[1])
}

// An agent that corrupts a file but keeps its size and modification time
// must not survive the documented copy-back: restore gives the good copy
// the recorded mtime, so rsync's default quick check (size and mtime)
// would see nothing to do.
func TestDocumentedCopyBackRevertsSameSizeSameMtimeDamage(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("no rsync")
	}
	e := newSnapEnv(t)
	good := []byte("deploy_key = AAAA-good\n")
	bad := []byte("deploy_key = EVIL-bad!\n")
	f := filepath.Join(e.tree, "config.txt")
	mt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.WriteFile(f, good, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(f, mt, mt)
	h1 := e.snap(t)
	os.WriteFile(f, bad, 0o600)
	os.Chtimes(f, mt, mt)
	e.snap(t)

	s := e.store(t)
	target := filepath.Join(filepath.Dir(e.tree), "restored")
	rep, err := Restore(context.Background(), RestoreOptions{
		Target: target, Manifest: s.Open(h1.ID), Blobs: s,
		Forbidden: []string{e.tree, e.state}, InsideFD: fsutil.InsideByIdentityFD,
	})
	if err != nil || !rep.OK() {
		t.Fatalf("restore: %v %+v", err, rep)
	}
	argv := append(copyBackCommand(t), target+"/", e.tree+"/")
	if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(argv, " "), err, out)
	}
	if got, _ := os.ReadFile(f); string(got) != string(good) {
		t.Fatalf("after %q the tree still holds %q", strings.Join(argv[:len(argv)-2], " "), got)
	}
}

// The operations guide gives the same command as the warning.
func TestCopyBackCommandMatchesTheGuide(t *testing.T) {
	guide, err := os.ReadFile("../../docs-site/_guides/operations.md")
	if err != nil {
		t.Skip(err)
	}
	cmd := strings.Join(copyBackCommand(t), " ")
	if !strings.Contains(string(guide), "`"+cmd+" ") && !strings.Contains(string(guide), "`"+cmd+"`") {
		t.Fatalf("the operations guide does not give %q", cmd)
	}
}
