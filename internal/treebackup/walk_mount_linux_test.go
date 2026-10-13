//go:build linux

package treebackup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const usernsEnv = "LEVER_TREEBACKUP_TEST_USERNS"

// inUserns re-runs the named test in a fresh user and mount namespace,
// where it may mount. It reports whether the caller is that inner run.
func inUserns(t *testing.T, name string) bool {
	t.Helper()
	if os.Getenv(usernsEnv) == "1" {
		return true
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("unshare not available")
	}
	if out, err := exec.Command("unshare", "-rm", "true").CombinedOutput(); err != nil {
		t.Skipf("unshare -rm does not work here: %v %s", err, out)
	}
	cmd := exec.Command("unshare", "-rm", os.Args[0], "-test.run=^"+name+"$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), usernsEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: "+name) {
		t.Fatalf("in a user namespace: %v\n%s", err, out)
	}
	return false
}

func mount(t *testing.T, src, dst, fstype string, flags uintptr) {
	t.Helper()
	if err := unix.Mount(src, dst, fstype, flags, ""); err != nil {
		t.Fatalf("mount %s on %s: %v", src, dst, err)
	}
	t.Cleanup(func() { unix.Unmount(dst, unix.MNT_DETACH) })
}

func TestWalkMountsAreNotEntered(t *testing.T) {
	if !inUserns(t, "TestWalkMountsAreNotEntered") {
		return
	}
	tree, outside := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(outside, "host-secret"), "host data")
	writeFile(t, filepath.Join(tree, "bind/placeholder"), "")
	writeFile(t, filepath.Join(tree, "tmp/placeholder"), "")
	writeFile(t, filepath.Join(tree, "fbind"), "")
	writeFile(t, filepath.Join(tree, "plain"), "p")
	mount(t, outside, filepath.Join(tree, "bind"), "", unix.MS_BIND)
	mount(t, "tmpfs", filepath.Join(tree, "tmp"), "tmpfs", 0)
	writeFile(t, filepath.Join(tree, "tmp/x"), "tmpfs data")
	mount(t, filepath.Join(outside, "host-secret"), filepath.Join(tree, "fbind"), "", unix.MS_BIND)

	sink := newMemSink()
	o := walkOpts(tree, sink)
	res := mustWalk(t, o)
	for _, p := range []string{"bind", "tmp", "fbind"} {
		if e := mustFind(t, res, p); e.State != StateMount || e.SHA256 != "" {
			t.Errorf("%s: %+v", p, e)
		}
	}
	for _, e := range res.Entries {
		if IsUnder(e.Path, []byte("bind")) || IsUnder(e.Path, []byte("tmp")) {
			t.Errorf("entered a mount: %s", Q(e.Path))
		}
	}
	for _, data := range []string{"host data", "tmpfs data"} {
		if sink.has(sumOf([]byte(data))) {
			t.Errorf("read %q through a mount", data)
		}
	}
	if mustFind(t, res, "plain").SHA256 == "" {
		t.Errorf("plain file not captured")
	}

	// A bind-mounted file is a mount even before any open, whatever its size.
	o.Limits.File = 1
	res = mustWalk(t, o)
	if e := mustFind(t, res, "fbind"); e.State != StateMount {
		t.Errorf("fbind over the file limit: %+v", e)
	}
	o.Limits = testLimits()

	// A known base that walked through the paths before they were mounts.
	o.Known = &Base{Header: &Header{}, Entries: []Entry{
		{Path: []byte("bind"), Type: TypeDir}, {Path: []byte("bind/host-secret"), Type: TypeFile},
	}}
	res = mustWalk(t, o)
	if find(res, "bind/host-secret") != nil {
		t.Errorf("phase A probed through a mount")
	}
}

func TestWalkGuardCatchesBindMountedStateDir(t *testing.T) {
	if !inUserns(t, "TestWalkGuardCatchesBindMountedStateDir") {
		return
	}
	tree, state := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(state, "ledger"), "secret")
	writeFile(t, filepath.Join(tree, "deep/alias/placeholder"), "")
	mount(t, state, filepath.Join(tree, "deep/alias"), "", unix.MS_BIND)
	var st syscall.Stat_t
	if err := syscall.Stat(state, &st); err != nil {
		t.Fatal(err)
	}
	o := walkOpts(tree, newMemSink())
	o.Guard = []DevIno{{uint64(st.Dev), uint64(st.Ino)}}
	if _, err := Walk(context.Background(), o); !errors.Is(err, ErrStateInTree) {
		t.Fatalf("Walk: %v", err)
	}
}

func openFDs(t *testing.T) int {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd")
	}
	return len(ents)
}

func TestWalkOpenFDsStayBounded(t *testing.T) {
	resetSeams(t)
	tree := t.TempDir()
	deep := tree
	const depth = 110
	for range depth {
		deep = filepath.Join(deep, "d")
		writeFile(t, filepath.Join(deep, "f"), "x")
	}
	for i := range 200 {
		writeFile(t, filepath.Join(tree, fmt.Sprintf("w%03d/f", i)), "y")
	}
	base := openFDs(t)
	peak := 0
	sample := func([]byte) { peak = max(peak, openFDs(t)-base) }
	walkSeams.beforeOpen = sample
	walkSeams.beforeList = sample
	o := walkOpts(tree, newMemSink())
	first := mustWalk(t, o)
	o.Known = baseOf(first)
	mustWalk(t, o)
	if limit := depth + dirCacheSize + 8; peak > limit {
		t.Errorf("peak of %d fds over the bound %d", peak, limit)
	}
	if n := openFDs(t) - base; n > 0 {
		t.Errorf("%d fds leaked", n)
	}
	t.Logf("peak %d extra fds", peak)
}
