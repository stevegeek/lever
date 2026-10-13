//go:build linux || darwin

package treebackup

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const testMtime = int64(1_600_000_000_123_456_000)

type rtree struct {
	entries []Entry
	blobs   memBlobs
	root    RootInfo
	err     error
}

func newRTree() *rtree {
	return &rtree{blobs: memBlobs{}, root: RootInfo{HasMeta: true, Mode: 0o755, Mtime: testMtime}}
}

func (t *rtree) add(e Entry) *rtree { t.entries = append(t.entries, e); return t }

func (t *rtree) dir(p string, mode uint32) *rtree {
	return t.add(Entry{Path: []byte(p), Type: TypeDir, HasMeta: true, Mode: mode, Mtime: testMtime})
}

func (t *rtree) file(p, content string, mode uint32) *rtree {
	s := sumOf([]byte(content))
	t.blobs[s] = []byte(content)
	return t.add(Entry{Path: []byte(p), Type: TypeFile, HasMeta: true, Mode: mode, Mtime: testMtime,
		Size: int64(len(content)), SHA256: s, Verified: 1})
}

func (t *rtree) link(p, target string) *rtree {
	return t.add(Entry{Path: []byte(p), Type: TypeLink, HasMeta: true, Mode: 0o777, Mtime: testMtime,
		Target: []byte(target), HasTarget: true})
}

func (t *rtree) manifest() OpenManifest {
	h := &Header{Version: ManifestVersion, Root: t.root}
	m := memManifest(h, t.entries)
	if t.err == nil {
		return m
	}
	return func() (EntryReader, error) {
		rd, _ := m()
		rd.(*memReader).err = t.err
		return rd, nil
	}
}

func (t *rtree) opts(target string) RestoreOptions {
	return RestoreOptions{Target: target, Manifest: t.manifest(), Blobs: t.blobs, Inside: insideByStat}
}

// insideByStat is a test stand-in for the identity check: it walks up from
// inner with os.Stat and compares with os.SameFile.
func insideByStat(inner, outer string) (bool, error) {
	o, err := os.Stat(outer)
	if err != nil {
		return false, err
	}
	p, err := filepath.EvalSymlinks(inner)
	if err != nil {
		return false, err
	}
	for {
		st, err := os.Stat(p)
		if err != nil {
			return false, err
		}
		if os.SameFile(st, o) {
			return true, nil
		}
		up := filepath.Dir(p)
		if up == p {
			return false, nil
		}
		p = up
	}
}

func withFS(t *testing.T, wrap func(restoreFS) restoreFS) {
	t.Helper()
	restoreFSWrap = wrap
	t.Cleanup(func() { restoreFSWrap = nil })
}

func restore(t *testing.T, o RestoreOptions) *RestoreReport {
	t.Helper()
	rep, err := Restore(context.Background(), o)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Lstat(o.Target + partialSuffix); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("partial directory left behind: %v", err)
	}
	return rep
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustNotExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s exists (err %v)", p, err)
	}
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

func TestRestoreWritesTreeWithPrivateModesAndMtimes(t *testing.T) {
	tr := newRTree().dir("d", 0o755).file("d/f", "hello\n", 0o644).file("top", "", 0o755).
		dir("d/e", 0o755).file("d/e/g", strings.Repeat("x", 3<<20), 0o600).link("d/l", "f").
		file("hard1", "same", 0o644).file("hard2", "same", 0o644)
	target := filepath.Join(t.TempDir(), "out")
	rep := restore(t, tr.opts(target))
	if !rep.OK() || rep.Files != 5 || rep.Dirs != 2 || rep.Links != 1 {
		t.Fatalf("report %+v", rep)
	}
	if got := readFile(t, filepath.Join(target, "d/f")); got != "hello\n" {
		t.Fatalf("d/f = %q", got)
	}
	if got := readFile(t, filepath.Join(target, "d/l")); got != "hello\n" {
		t.Fatalf("d/l = %q", got)
	}
	if len(readFile(t, filepath.Join(target, "d/e/g"))) != 3<<20 {
		t.Fatal("d/e/g size")
	}
	if m := mode(t, filepath.Join(target, "top")); m.Perm() != 0o600 {
		t.Fatalf("top mode %v, want 0600 without KeepModes", m)
	}
	if m := mode(t, filepath.Join(target, "d")); m.Perm() != 0o700 {
		t.Fatalf("d mode %v", m)
	}
	a, _ := os.Stat(filepath.Join(target, "hard1"))
	b, _ := os.Stat(filepath.Join(target, "hard2"))
	if os.SameFile(a, b) {
		t.Fatal("hard links must restore as separate files")
	}
	for _, p := range []string{"", "d", "d/f", "d/e", "d/e/g", "top"} {
		fi, err := os.Lstat(filepath.Join(target, p))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.ModTime().UnixNano(); got/1000 != testMtime/1000 {
			t.Errorf("%q mtime %d, want %d", p, got, testMtime)
		}
	}
	fi, _ := os.Lstat(filepath.Join(target, "d/l"))
	if got := fi.ModTime().UnixNano(); got != testMtime {
		t.Errorf("link mtime %d", got)
	}
	var out bytes.Buffer
	rep.Write(&out)
	if !strings.Contains(out.String(), "restored 5 files, 2 directories, 1 links") || !strings.Contains(out.String(), RestoreWarning) {
		t.Fatalf("report text:\n%s", out.String())
	}
}

func TestRestoreKeepModesStripsSBitsAndReportsThem(t *testing.T) {
	tr := newRTree().dir("tmp", 0o1777).file("tmp/suid", "x", 0o4755).file("sgid", "y", 0o2750).file("ro", "z", 0o400).
		dir("closed", 0o500).file("closed/in", "w", 0o644)
	target := filepath.Join(t.TempDir(), "out")
	o := tr.opts(target)
	o.KeepModes = true
	rep := restore(t, o)
	t.Cleanup(func() { os.Chmod(filepath.Join(target, "closed"), 0o700) })
	want := map[string]os.FileMode{"tmp": 0o777, "tmp/suid": 0o755, "sgid": 0o750, "ro": 0o400, "closed": 0o500, "closed/in": 0o644}
	for p, w := range want {
		m := mode(t, filepath.Join(target, p))
		if m.Perm() != w || m&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			t.Errorf("%s mode %v, want %v", p, m, w)
		}
	}
	if len(rep.StrippedSBits) != 3 || !rep.OK() {
		t.Fatalf("stripped %q, OK %v", rep.StrippedSBits, rep.OK())
	}
	if got := readFile(t, filepath.Join(target, "closed/in")); got != "w" {
		t.Fatal("closed/in content")
	}
}

func TestRestoreRefusesAnExistingTargetOrPartial(t *testing.T) {
	tr := newRTree().file("f", "x", 0o644)
	for _, existing := range []string{"out", "out" + partialSuffix} {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, existing), 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, "out")
		_, err := Restore(context.Background(), tr.opts(target))
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("%s: err %v", existing, err)
		}
		ents, _ := os.ReadDir(dir)
		if len(ents) != 1 {
			t.Fatalf("%s: something was created: %v", existing, ents)
		}
	}
	// A dangling link at the target name also counts as existing.
	dir := t.TempDir()
	os.Symlink("nowhere", filepath.Join(dir, "out"))
	if _, err := Restore(context.Background(), tr.opts(filepath.Join(dir, "out"))); err == nil {
		t.Fatal("restore over a dangling link")
	}
}

func TestRestoreRefusesTargetInsideTreeStoreOrStateByIdentity(t *testing.T) {
	base := t.TempDir()
	tree, store, state := filepath.Join(base, "tree"), filepath.Join(base, "state/tree-backups"), filepath.Join(base, "state")
	for _, d := range []string{tree + "/sub", store} {
		os.MkdirAll(d, 0o700)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(tree+"/sub", alias); err != nil {
		t.Fatal(err)
	}
	tr := newRTree().file("f", "x", 0o644)
	for _, target := range []string{tree + "/out", tree + "/sub/out", alias + "/out", store + "/out", state + "/out"} {
		o := tr.opts(target)
		o.Forbidden = []string{tree, store, state}
		_, err := Restore(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), "inside") {
			t.Fatalf("%s: err %v", target, err)
		}
		mustNotExist(t, target)
		mustNotExist(t, target+partialSuffix)
	}
	o := tr.opts(filepath.Join(base, "out"))
	o.Forbidden = []string{tree}
	o.Inside = func(string, string) (bool, error) { return false, errors.New("no proof") }
	if _, err := Restore(context.Background(), o); err == nil || !strings.Contains(err.Error(), "cannot prove") {
		t.Fatalf("an identity error must refuse: %v", err)
	}
	o.Inside = nil
	if _, err := Restore(context.Background(), o); err == nil {
		t.Fatal("no identity check must refuse")
	}
	mustNotExist(t, filepath.Join(base, "out"+partialSuffix))
	o = tr.opts(filepath.Join(base, "out"))
	o.Forbidden = []string{tree, store, state}
	restore(t, o)
}

func TestRestoreHashMismatchUnlinksReportsAndContinues(t *testing.T) {
	tr := newRTree().file("bad", "recorded", 0o644).file("good", "fine", 0o644).file("missing", "gone", 0o644).
		file("short", "longer content", 0o644)
	tr.blobs[tr.entries[0].SHA256] = []byte("tampered")
	delete(tr.blobs, tr.entries[2].SHA256)
	tr.blobs[tr.entries[3].SHA256] = []byte("longer")
	target := filepath.Join(t.TempDir(), "out")
	rep := restore(t, tr.opts(target))
	if rep.OK() || len(rep.HashFailures) != 3 || rep.Files != 1 {
		t.Fatalf("report %+v", rep)
	}
	for _, p := range []string{"bad", "missing", "short"} {
		mustNotExist(t, filepath.Join(target, p))
	}
	if readFile(t, filepath.Join(target, "good")) != "fine" {
		t.Fatal("good not restored")
	}
}

func TestRestorePathSubsetWithAncestors(t *testing.T) {
	tr := newRTree().dir("a", 0o755).dir("a/b", 0o755).file("a/b/c", "c", 0o644).dir("a/b/c2", 0o755).
		file("a/b/c2/x", "x", 0o644).file("a/side", "s", 0o644).file("other", "o", 0o644).dir("a/b/cc", 0o755).
		file("a/b/cc/y", "y", 0o644)
	target := filepath.Join(t.TempDir(), "out")
	o := tr.opts(target)
	o.Paths = [][]byte{[]byte("a/b/c2"), []byte("other")}
	rep := restore(t, o)
	if !rep.OK() || rep.Files != 2 || rep.Dirs != 3 {
		t.Fatalf("report %+v", rep)
	}
	if readFile(t, filepath.Join(target, "a/b/c2/x")) != "x" || readFile(t, filepath.Join(target, "other")) != "o" {
		t.Fatal("selected files missing")
	}
	for _, p := range []string{"a/b/c", "a/side", "a/b/cc"} {
		mustNotExist(t, filepath.Join(target, p))
	}
	o = tr.opts(filepath.Join(t.TempDir(), "out"))
	o.Paths = [][]byte{[]byte("a/nope")}
	if _, err := Restore(context.Background(), o); err == nil || !strings.Contains(err.Error(), "not in the snapshot") {
		t.Fatalf("unknown path: %v", err)
	}
	mustNotExist(t, o.Target+partialSuffix)
	o.Paths = [][]byte{[]byte("a/../other")}
	if _, err := Restore(context.Background(), o); err == nil {
		t.Fatal("a path with .. was accepted")
	}
}

type failingFS struct {
	restoreFS
	n, after int
}

func (f *failingFS) createFile(dirfd int, name string) (int, error) {
	f.n++
	if f.n > f.after {
		return -1, syscall.EIO
	}
	return f.restoreFS.createFile(dirfd, name)
}

func TestRestoreCrashLeavesPartial(t *testing.T) {
	tr := newRTree().file("a", "1", 0o644).file("b", "2", 0o644).file("c", "3", 0o644)
	target := filepath.Join(t.TempDir(), "out")
	withFS(t, func(fs restoreFS) restoreFS { return &failingFS{restoreFS: fs, after: 1} })
	_, err := Restore(context.Background(), tr.opts(target))
	if err == nil || !strings.Contains(err.Error(), partialSuffix) {
		t.Fatalf("err %v", err)
	}
	mustNotExist(t, target)
	if readFile(t, filepath.Join(target+partialSuffix, "a")) != "1" {
		t.Fatal("partial content")
	}
	if _, err := Restore(context.Background(), tr.opts(target)); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a second restore over the partial: %v", err)
	}
}

type cancelFS struct {
	restoreFS
	cancel func()
}

func (f cancelFS) createFile(dirfd int, name string) (int, error) {
	if name == "b" {
		f.cancel()
	}
	return f.restoreFS.createFile(dirfd, name)
}

func TestRestoreInterruptLeavesPartial(t *testing.T) {
	tr := newRTree().file("a", "1", 0o644).file("b", "2", 0o644).file("c", "3", 0o644)
	target := filepath.Join(t.TempDir(), "out")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	withFS(t, func(fs restoreFS) restoreFS { return cancelFS{fs, cancel} })
	_, err := Restore(ctx, tr.opts(target))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	mustNotExist(t, target)
	mustNotExist(t, filepath.Join(target+partialSuffix, "c"))
}

func TestRestoreInvalidManifestCreatesNothing(t *testing.T) {
	tr := newRTree().file("a", "1", 0o644)
	tr.err = errors.Join(ErrInvalid, errors.New("trailer mismatch"))
	dir := t.TempDir()
	if _, err := Restore(context.Background(), tr.opts(filepath.Join(dir, "out"))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("created %v", ents)
	}
}

func TestRestoreRefusesWhenFreeSpaceIsShort(t *testing.T) {
	tr := newRTree().file("a", "1", 0o644)
	dir := t.TempDir()
	o := tr.opts(filepath.Join(dir, "out"))
	o.MinFree = 1 << 62
	if _, err := Restore(context.Background(), o); err == nil || !strings.Contains(err.Error(), "free space") {
		t.Fatalf("err %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("created %v", ents)
	}
}

func TestRestoreNonUTF8NamesRestoreAsTwoFiles(t *testing.T) {
	tr := newRTree().file("x\xff", "ff", 0o644).file("x\xfe", "fe", 0o644).dir("d\xff", 0o755).file("d\xff/in", "in", 0o644)
	target := filepath.Join(t.TempDir(), "out")
	rep := restore(t, tr.opts(target))
	if !rep.OK() || rep.Files != 3 {
		t.Fatalf("report %+v", rep)
	}
	if readFile(t, filepath.Join(target, "x\xff")) != "ff" || readFile(t, filepath.Join(target, "x\xfe")) != "fe" {
		t.Fatal("contents")
	}
}

func TestRestoreNamesTheTargetRefusesAreReported(t *testing.T) {
	withFS(t, func(fs restoreFS) restoreFS { return utf8OnlyFS{fs} })
	tr := newRTree().file("x\xff", "ff", 0o644).dir("d\xfe", 0o755).file("d\xfe/in", "in", 0o644).file("ok", "ok", 0o644).
		link("l\xff", "ok")
	target := filepath.Join(t.TempDir(), "out")
	rep := restore(t, tr.opts(target))
	if rep.OK() || len(rep.BadNames) != 3 || rep.Files != 1 {
		t.Fatalf("report %+v", rep)
	}
	if !strings.Contains(strings.Join(rep.BadNames, "\n"), `"x\xff"`) {
		t.Fatalf("bad name lines %q", rep.BadNames)
	}
	ents, _ := os.ReadDir(target)
	if len(ents) != 1 || ents[0].Name() != "ok" {
		t.Fatalf("target holds %v", ents)
	}
}

func TestRestoreFoldingCollisionsSkipTheLaterSubtree(t *testing.T) {
	withFS(t, func(fs restoreFS) restoreFS { return foldingFS{fs} })
	tr := newRTree().
		dir("Dir", 0o755).file("Dir/x", "upper", 0o644).
		dir("dir", 0o755).file("dir/x", "lower", 0o644).dir("dir/deep", 0o755).file("dir/deep/y", "y", 0o644).
		file("README", "R", 0o644).file("readme", "r", 0o644).
		file("L", "file", 0o644).link("l", "README")
	target := filepath.Join(t.TempDir(), "out")
	rep := restore(t, tr.opts(target))
	if rep.OK() || len(rep.Collisions) != 3 || len(rep.ForeignWriters) != 0 {
		t.Fatalf("report %+v", rep)
	}
	for _, c := range rep.Collisions {
		if !strings.Contains(c, " and ") {
			t.Fatalf("collision line %q names one entry", c)
		}
	}
	if readFile(t, filepath.Join(target, "Dir/x")) != "upper" || readFile(t, filepath.Join(target, "README")) != "R" ||
		readFile(t, filepath.Join(target, "L")) != "file" {
		t.Fatal("the first of each pair must be kept untouched")
	}
	ents, _ := os.ReadDir(filepath.Join(target, "Dir"))
	if len(ents) != 1 {
		t.Fatalf("merged into Dir: %v", ents)
	}
	if fi, _ := os.Lstat(filepath.Join(target, "L")); !fi.Mode().IsRegular() {
		t.Fatal("L was replaced")
	}
}

// foreignFS plays a host process (Finder, an indexer) that drops a file into
// a directory as soon as lever creates it.
type foreignFS struct{ restoreFS }

func (f foreignFS) mkdirat(dirfd int, name string) error {
	if err := f.restoreFS.mkdirat(dirfd, name); err != nil {
		return err
	}
	fd, err := f.restoreFS.openDir(dirfd, name)
	if err != nil {
		return err
	}
	defer f.restoreFS.close(fd)
	ffd, err := f.restoreFS.createFile(fd, ".DS_Store")
	if err != nil {
		return err
	}
	return f.restoreFS.close(ffd)
}

func TestRestoreForeignWriterInTheTargetIsReported(t *testing.T) {
	withFS(t, func(fs restoreFS) restoreFS { return foreignFS{fs} })
	tr := newRTree().dir("d", 0o755).file("d/.DS_Store", "agent", 0o644).file("d/x", "x", 0o644)
	target := filepath.Join(t.TempDir(), "out")
	rep := restore(t, tr.opts(target))
	if rep.OK() || len(rep.ForeignWriters) != 1 || len(rep.Collisions) != 0 || rep.Files != 1 {
		t.Fatalf("report %+v", rep)
	}
	if readFile(t, filepath.Join(target, "d/.DS_Store")) != "" {
		t.Fatal("the foreign file was overwritten")
	}
}

// hookFS runs before the n-th directory open, counting from 1.
type hookFS struct {
	restoreFS
	n      int
	at     int
	before func()
}

func (h *hookFS) openDir(dirfd int, name string) (int, error) {
	h.n++
	if h.n == h.at {
		h.before()
	}
	return h.restoreFS.openDir(dirfd, name)
}

func TestRestoreDirectorySwappedForLinkIsNotFollowed(t *testing.T) {
	// Opens: 1 the new root, 2 "d" in phase 1, 3 "d" again in phase 2, 4 in
	// phase 3.
	for _, at := range []int{3, 4} {
		base := t.TempDir()
		outside := filepath.Join(base, "outside")
		os.Mkdir(outside, 0o700)
		tr := newRTree().dir("d", 0o755).file("d/f", "f", 0o644).link("d/l", "f").link("z", "d")
		target := filepath.Join(base, "out")
		o := tr.opts(target)
		o.Links = LinksAll
		withFS(t, func(inner restoreFS) restoreFS {
			return &hookFS{restoreFS: inner, at: at, before: func() {
				p := target + partialSuffix
				if err := os.Rename(filepath.Join(p, "d"), filepath.Join(base, "moved")); err != nil {
					t.Error(err)
				}
				if err := os.Symlink(outside, filepath.Join(p, "d")); err != nil {
					t.Error(err)
				}
			}}
		})
		if _, err := Restore(context.Background(), o); err == nil {
			t.Fatalf("open %d: restore went on through a swapped directory", at)
		}
		if ents, _ := os.ReadDir(outside); len(ents) != 0 {
			t.Fatalf("open %d: wrote outside: %v", at, ents)
		}
		mustNotExist(t, target)
	}
}

func TestRestoreLinksInsideRule(t *testing.T) {
	tr := newRTree().dir("s", 0o755).dir("s/b", 0o755).file("s/b/f", "f", 0o644).
		link("s/a", "b/../..").link("s/up", "..").link("s/up2", "../..").link("s/abs", "/etc").
		link("s/dot", "./b").link("s/ok", "b/f").link("s/okdir", "b/").link("s/empty", "").
		link("top", "s/b/f").link("topup", "..").
		dir("s/b/c", 0o755).link("s/b/c/l", "../../../s/b/f").link("s/b/c/l4", "../../../..")
	tr.add(Entry{Path: []byte("s/noTarget"), Type: TypeLink, State: StateUnreadable})
	target := filepath.Join(t.TempDir(), "out")
	rep := restore(t, tr.opts(target))
	created := []string{"s/up", "s/ok", "s/okdir", "top", "s/b/c/l"}
	refused := []string{"s/a", "s/up2", "s/abs", "s/dot", "s/empty", "topup", "s/b/c/l4", "s/noTarget"}
	for _, p := range created {
		if fi, err := os.Lstat(filepath.Join(target, p)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s not created: %v", p, err)
		}
	}
	for _, p := range refused {
		mustNotExist(t, filepath.Join(target, p))
	}
	if rep.OK() || len(rep.LinksNotCreated) != len(refused) || rep.Links != int64(len(created)) {
		t.Fatalf("report %+v", rep)
	}
	if readFile(t, filepath.Join(target, "s/b/c/l")) != "f" {
		t.Fatal("s/b/c/l does not resolve")
	}
}

func TestRestoreLinkDepthCountsFromTheTargetRootWithPath(t *testing.T) {
	tr := newRTree().dir("deep", 0o755).dir("deep/er", 0o755).link("deep/er/l", "../..").link("deep/er/l3", "../../..").
		file("other", "o", 0o644)
	target := filepath.Join(t.TempDir(), "out")
	o := tr.opts(target)
	o.Paths = [][]byte{[]byte("deep/er/l"), []byte("deep/er/l3")}
	rep := restore(t, o)
	if fi, err := os.Lstat(filepath.Join(target, "deep/er/l")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("deep/er/l not created: %v", err)
	}
	mustNotExist(t, filepath.Join(target, "deep/er/l3"))
	if len(rep.LinksNotCreated) != 1 {
		t.Fatalf("report %+v", rep)
	}
}

func TestRestoreLinksAllCreatesAsRecordedWithoutFollowing(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	os.Mkdir(outside, 0o700)
	withFS(t, func(fs restoreFS) restoreFS { return foldingFS{fs} })
	tr := newRTree().link("D", outside).dir("d", 0o755).file("d/f", "f", 0o644).link("s", "b/../..").
		link("abs", "/etc/passwd")
	target := filepath.Join(base, "out")
	o := tr.opts(target)
	o.Links = LinksAll
	rep := restore(t, o)
	for p, want := range map[string]string{"s": "b/../..", "abs": "/etc/passwd"} {
		if got, err := os.Readlink(filepath.Join(target, p)); err != nil || got != want {
			t.Errorf("%s -> %q (%v)", p, got, err)
		}
	}
	if len(rep.Collisions) != 1 {
		t.Fatalf("report %+v", rep)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("wrote through a link: %v", ents)
	}
	if readFile(t, filepath.Join(target, "d/f")) != "f" {
		t.Fatal("d/f")
	}
}

func TestRestoreLinksNoneCreatesNone(t *testing.T) {
	tr := newRTree().file("f", "f", 0o644).link("l", "f").link("m", "..")
	target := filepath.Join(t.TempDir(), "out")
	o := tr.opts(target)
	o.Links = LinksNone
	rep := restore(t, o)
	mustNotExist(t, filepath.Join(target, "l"))
	mustNotExist(t, filepath.Join(target, "m"))
	if rep.OK() || rep.Links != 0 || len(rep.LinksNotCreated) != 2 {
		t.Fatalf("report %+v", rep)
	}
	o.Links = "some"
	if _, err := Restore(context.Background(), o); err == nil {
		t.Fatal("unknown policy accepted")
	}
}

func TestRestoreReportsSpecialsAndEntriesWithoutContent(t *testing.T) {
	tr := newRTree().
		add(Entry{Path: []byte(".lever"), Type: TypeDir}).
		add(Entry{Path: []byte(".lever/bootstrap.json"), Type: TypeFile, HasMeta: true, Size: 5, SHA256: sumOf([]byte("tickt")), Verified: 1, State: StateSecret}).
		add(Entry{Path: []byte("big"), Type: TypeFile, HasMeta: true, Size: 1 << 40, State: StateTooBig}).
		add(Entry{Path: []byte("fifo"), Type: TypeFIFO, HasMeta: true}).
		add(Entry{Path: []byte("sock"), Type: TypeSocket, HasMeta: true}).
		add(Entry{Path: []byte("dev"), Type: TypeChar, HasMeta: true}).
		add(Entry{Path: []byte("mnt"), Type: TypeDir, HasMeta: true, State: StateMount}).
		add(Entry{Path: []byte("trunc"), Type: TypeDir, HasMeta: true, State: StateTruncated, Reason: ReasonEntries}).
		add(Entry{Path: []byte("hidden"), Type: TypeDir, HasMeta: true, Listing: ListingUnreadable}).
		add(Entry{Path: []byte("racy"), Type: TypeDir, HasMeta: true, State: StateListingUnstable}).
		add(Entry{Path: []byte("skipped"), Type: TypeFile, HasMeta: true, Size: 3, State: StateSkipped, Reason: ReasonRead}).
		add(Entry{Path: []byte("what"), Type: TypeUnknown, State: StateUnreadable})
	tr.file("torn", "torn", 0o644)
	tr.entries[len(tr.entries)-1].State = StateUnstable
	tr.root.State, tr.root.Reason = StateTruncated, ReasonTime
	target := filepath.Join(t.TempDir(), "out")
	rep := restore(t, tr.opts(target))
	if len(rep.SpecialsNotCreated) != 3 || len(rep.NoContent) != 8 || len(rep.Unstable) != 2 || rep.OK() {
		t.Fatalf("report %+v", rep)
	}
	for _, p := range []string{".lever/bootstrap.json", "big", "fifo", "sock", "dev", "skipped", "what"} {
		mustNotExist(t, filepath.Join(target, p))
	}
	for _, p := range []string{"mnt", "trunc", "hidden", "racy"} {
		if fi, err := os.Stat(filepath.Join(target, p)); err != nil || !fi.IsDir() {
			t.Errorf("%s: %v", p, err)
		}
	}
	if readFile(t, filepath.Join(target, "torn")) != "torn" {
		t.Fatal("unstable file not restored")
	}
	var out bytes.Buffer
	rep.Write(&out)
	for _, s := range []string{"special files not created (3)", "entries with no content (8)", "secret", "unstable"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("report text lacks %q:\n%s", s, out.String())
		}
	}
}

func TestRestoreReportRendersHostileNamesQuoted(t *testing.T) {
	tr := newRTree().link("evil\x1b[2J\n", "/etc/passwd")
	target := filepath.Join(t.TempDir(), "out")
	rep := restore(t, tr.opts(target))
	var out bytes.Buffer
	rep.Write(&out)
	if strings.ContainsAny(out.String(), "\x1b") || !strings.Contains(out.String(), `\x1b`) {
		t.Fatalf("unescaped name in:\n%q", out.String())
	}
}

func TestGitKeyMatchesFoldedSpellings(t *testing.T) {
	for name, want := range map[string]string{
		"HEAD": "head", "HE­AD": "head", "conﬁg": "config", ".gi‍t": ".git",
		"hooKs": "hooks", "ｃommondir": "commondir", "cönfig": "config", "Config.Worktree": "config.worktree",
	} {
		if got := gitKey(name); got != want {
			t.Errorf("gitKey(%q) = %q, want %q", name, got, want)
		}
	}
	if gitKey("x\xffconfig") != "xconfig" || isGitControl("configs") {
		t.Error("gitKey matches too much")
	}
}
