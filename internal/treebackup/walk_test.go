//go:build unix

package treebackup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

func TestWalkRecordsTypesModesAndContent(t *testing.T) {
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "a/b.txt"), "hello")
	writeFile(t, filepath.Join(tree, "x\xff"), "ff")
	writeFile(t, filepath.Join(tree, "x\xfe"), "fe")
	if err := os.Symlink("../../etc/passwd", filepath.Join(tree, "a/evil")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(tree, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Chmod(filepath.Join(tree, "a/b.txt"), 0o4755); err != nil {
		t.Fatal(err)
	}
	sink := newMemSink()
	res := mustWalk(t, walkOpts(tree, sink))
	if !complete(res) {
		t.Fatalf("not complete: %+v %s", res.Root, dumpPaths(res))
	}
	if len(res.Entries) != 6 {
		t.Fatalf("entries: %s", dumpPaths(res))
	}
	b := mustFind(t, res, "a/b.txt")
	if b.Type != TypeFile || b.Mode != 0o4755 || b.SHA256 != sumOf([]byte("hello")) || b.Size != 5 || b.Verified == 0 {
		t.Errorf("a/b.txt: %+v", b)
	}
	if string(sink.blobs[b.SHA256]) != "hello" {
		t.Errorf("blob of a/b.txt is %q", sink.blobs[b.SHA256])
	}
	l := mustFind(t, res, "a/evil")
	if l.Type != TypeLink || !l.HasTarget || string(l.Target) != "../../etc/passwd" || l.SHA256 != "" {
		t.Errorf("a/evil: %+v", l)
	}
	if f := mustFind(t, res, "fifo"); f.Type != TypeFIFO || f.SHA256 != "" || f.State != StateOK {
		t.Errorf("fifo: %+v", f)
	}
	// Two names that differ only in an invalid byte stay two entries.
	ff, fe := mustFind(t, res, "x\xff"), mustFind(t, res, "x\xfe")
	if ff.SHA256 != sumOf([]byte("ff")) || fe.SHA256 != sumOf([]byte("fe")) {
		t.Errorf("x\\xff/x\\xfe: %+v %+v", ff, fe)
	}
	if res.Bytes.Total != 9 || res.Bytes.Read != 9 || res.Bytes.New != 9 || res.Blobs.New != 3 {
		t.Errorf("counts: %+v %+v", res.Bytes, res.Blobs)
	}
	st, _ := os.Stat(tree)
	if res.Root.Mode != uint32(st.Mode().Perm()) || res.TreeIno == 0 {
		t.Errorf("root: %+v", res.Root)
	}
}

func TestWalkResolvesTheOperatorPathOnce(t *testing.T) {
	real := t.TempDir()
	writeFile(t, filepath.Join(real, "f"), "x")
	link := filepath.Join(t.TempDir(), "tree")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	res := mustWalk(t, walkOpts(link, newMemSink()))
	mustFind(t, res, "f")
}

func TestWalkDirSwappedForLinkIsNotFollowed(t *testing.T) {
	for _, target := range []string{"b", "/etc"} {
		for _, phaseA := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/phaseA=%v", target, phaseA), func(t *testing.T) {
				resetSeams(t)
				tree := t.TempDir()
				writeFile(t, filepath.Join(tree, "a/inner"), "a")
				writeFile(t, filepath.Join(tree, "b/sibling"), "b")
				o := walkOpts(tree, newMemSink())
				if phaseA {
					o.Known = baseOf(mustWalk(t, o))
				}
				walkSeams.beforeOpen = func(rel []byte) {
					if string(rel) != "a" {
						return
					}
					walkSeams.beforeOpen = nil
					if err := os.Rename(filepath.Join(tree, "a"), filepath.Join(tree, "a.old")); err != nil {
						t.Error(err)
					}
					if err := os.Symlink(target, filepath.Join(tree, "a")); err != nil {
						t.Error(err)
					}
				}
				res := mustWalk(t, o)
				a := mustFind(t, res, "a")
				if a.Type != TypeDir || a.State != StateChanged {
					t.Errorf("a: %+v", a)
				}
				for _, e := range res.Entries {
					if IsUnder(e.Path, []byte("a")) {
						t.Errorf("recorded through the swapped link: %s", Q(e.Path))
					}
				}
			})
		}
	}
}

func TestWalkMovedDirectoryIsRecordedOnce(t *testing.T) {
	resetSeams(t)
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "a/x/f"), "f")
	if err := os.Mkdir(filepath.Join(tree, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	walkSeams.beforeList = func(rel []byte) {
		if string(rel) == "b" {
			if err := os.Rename(filepath.Join(tree, "a/x"), filepath.Join(tree, "b/x")); err != nil {
				t.Error(err)
			}
		}
	}
	res := mustWalk(t, walkOpts(tree, newMemSink()))
	if e := mustFind(t, res, "a/x/f"); e.SHA256 == "" && e.State != StateChanged {
		t.Errorf("a/x/f: %+v", e)
	}
	bx := mustFind(t, res, "b/x")
	if bx.State != StateChanged {
		t.Errorf("b/x: %+v", bx)
	}
	if find(res, "b/x/f") != nil {
		t.Errorf("the moved directory was walked twice: %s", dumpPaths(res))
	}
}

func TestWalkFIFOSwappedBeforeOpenDoesNotHang(t *testing.T) {
	resetSeams(t)
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "f"), "content")
	if err := os.Mkdir(filepath.Join(tree, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	walkSeams.beforeOpen = func(rel []byte) {
		switch string(rel) {
		case "f", "d":
			p := filepath.Join(tree, string(rel))
			if err := os.RemoveAll(p); err != nil {
				t.Error(err)
			}
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	done := make(chan *WalkResult)
	go func() {
		res, err := Walk(context.Background(), walkOpts(tree, newMemSink()))
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res == nil {
			return
		}
		checkValid(t, res, testLimits())
		if e := mustFind(t, res, "f"); e.State != StateChanged || e.SHA256 != "" {
			t.Errorf("f: %+v", e)
		}
		if e := mustFind(t, res, "d"); e.State != StateChanged {
			t.Errorf("d: %+v", e)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the walk hung on a FIFO")
	}
}

func TestWalkFileChangingDuringReadIsUnstable(t *testing.T) {
	resetSeams(t)
	tree := t.TempDir()
	p := filepath.Join(tree, "grow")
	writeFile(t, p, strings.Repeat("a", 3<<20))
	writeFile(t, filepath.Join(tree, "calm"), "calm")
	attempts := 0
	walkSeams.afterChunk = func(rel []byte, attempt int, off int64) {
		if string(rel) != "grow" || off != 1<<20 {
			return
		}
		attempts = attempt
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Error(err)
			return
		}
		f.WriteString("appended")
		f.Close()
	}
	sink := newMemSink()
	res := mustWalk(t, walkOpts(tree, sink))
	g := mustFind(t, res, "grow")
	if g.State != StateUnstable || attempts != readAttempts {
		t.Fatalf("grow: %+v after %d attempts", g, attempts)
	}
	blob := sink.blobs[g.SHA256]
	if sumOf(blob) != g.SHA256 || int64(len(blob)) != g.Size {
		t.Errorf("blob does not match its entry: %d bytes, entry size %d", len(blob), g.Size)
	}
	if res.Bytes.Read < 3*(3<<20) {
		t.Errorf("retries not charged: read %d", res.Bytes.Read)
	}
	if c := mustFind(t, res, "calm"); c.State != StateOK || c.SHA256 != sumOf([]byte("calm")) {
		t.Errorf("calm: %+v", c)
	}
}

func TestWalkConcurrentRewriterBlobsMatchTheirHash(t *testing.T) {
	tree := t.TempDir()
	p := filepath.Join(tree, "f")
	writeFile(t, p, strings.Repeat("x", 2<<20))
	stop := make(chan struct{})
	done := make(chan struct{})
	defer func() { close(stop); <-done }()
	go func() {
		defer close(done)
		r := rand.New(rand.NewPCG(1, 2))
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			data := bytes.Repeat([]byte{byte('a' + i%26)}, (1<<20)+r.IntN(2<<20))
			os.WriteFile(p, data, 0o644)
		}
	}()
	sink := newMemSink()
	captured := 0
	defer func() {
		if captured == 0 {
			t.Errorf("no walk captured the file")
		}
	}()
	for range 10 {
		res := mustWalk(t, walkOpts(tree, sink))
		e := mustFind(t, res, "f")
		if e.SHA256 == "" {
			continue
		}
		captured++
		blob, ok := sink.blobs[e.SHA256]
		if !ok || sumOf(blob) != e.SHA256 || int64(len(blob)) != e.Size {
			t.Fatalf("f: entry %+v does not match its blob (%d bytes)", e, len(blob))
		}
	}
}

func TestWalkPermissionTricksAreRecorded(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "secret"), "s")
	writeFile(t, filepath.Join(tree, "p300/known"), "k")
	writeFile(t, filepath.Join(tree, "p600/known"), "k")
	sink := newMemSink()
	o := walkOpts(tree, sink)
	base := baseOf(mustWalk(t, o))
	writeFile(t, filepath.Join(tree, "p300/new"), "n")
	chmod := func(p string, m os.FileMode) {
		t.Helper()
		if err := os.Chmod(filepath.Join(tree, p), m); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(filepath.Join(tree, p), 0o755) })
	}
	chmod("secret", 0)
	chmod("p300", 0o300)
	chmod("p600", 0o600)
	o.Known, o.Cache = base, base
	res := mustWalk(t, o)
	if e := mustFind(t, res, "secret"); e.State != StateUnreadable || !e.HasMeta || e.Size != 1 {
		t.Errorf("secret: %+v", e)
	}
	if e := mustFind(t, res, "p300"); e.Listing != ListingUnreadable {
		t.Errorf("p300: %+v", e)
	}
	if e := mustFind(t, res, "p300/known"); e.SHA256 == "" {
		t.Errorf("p300/known not probed by name: %+v", e)
	}
	if find(res, "p300/new") != nil {
		t.Errorf("p300/new listed from an unlistable directory")
	}
	if e := mustFind(t, res, "p600"); e.Listing != ListingUnreadable {
		t.Errorf("p600: %+v", e)
	}
	if e := mustFind(t, res, "p600/known"); e.State != StateUnreadable {
		t.Errorf("p600/known: %+v", e)
	}
	if res.UnreadableNew != 2 {
		t.Errorf("unreadable new: %d", res.UnreadableNew)
	}
	if !complete(res) {
		t.Errorf("hidden content cleared complete: %+v", res.Root)
	}

	t.Run("root0300", func(t *testing.T) {
		chmod(".", 0o300)
		res := mustWalk(t, o)
		if res.Root.Listing != ListingUnreadable || res.Root.State != StateOK {
			t.Errorf("root: %+v", res.Root)
		}
		if find(res, "p300/known") == nil {
			t.Errorf("known paths not probed under a 0300 root: %s", dumpPaths(res))
		}
		if complete(res) {
			t.Errorf("complete with an unlistable root")
		}
	})
	t.Run("root0000", func(t *testing.T) {
		chmod(".", 0)
		res := mustWalk(t, o)
		if res.Root.State != StateUnreadable || len(res.Entries) != 0 || res.TreeIno == 0 {
			t.Errorf("root: %+v, %d entries", res.Root, len(res.Entries))
		}
	})
}

func TestWalkRenameRaceIsListingUnstable(t *testing.T) {
	resetSeams(t)
	tree := t.TempDir()
	for i := range 5000 {
		writeFile(t, filepath.Join(tree, fmt.Sprintf("d/f%04d", i)), "")
	}
	writeFile(t, filepath.Join(tree, "e/a"), "")
	writeFile(t, filepath.Join(tree, "e/b"), "")
	walkSeams.listed = func(rel []byte, chunk int, names []string) []string {
		switch {
		case string(rel) == "d" && chunk == 0 && len(names) > 0:
			// A name not read yet moves to one already read: getdents
			// may never return it.
			seen := map[string]bool{}
			for _, n := range names {
				seen[n] = true
			}
			for i := range 5000 {
				n := fmt.Sprintf("f%04d", i)
				if !seen[n] {
					os.Remove(filepath.Join(tree, "d", names[0]))
					if err := os.Rename(filepath.Join(tree, "d", n), filepath.Join(tree, "d", names[0])); err != nil {
						t.Error(err)
					}
					break
				}
			}
		case string(rel) == "e" && chunk == 0:
			// A name listed that is gone before its stat; the directory's
			// own times do not change.
			names = append(names, "ghost")
		}
		return names
	}
	res := mustWalk(t, walkOpts(tree, newMemSink()))
	for _, d := range []string{"d", "e"} {
		if e := mustFind(t, res, d); e.State != StateListingUnstable {
			t.Errorf("%s: %+v", d, e)
		}
	}
}

func TestWalkRenameFlipFlopNeverHidesAFileInAFullyListedDir(t *testing.T) {
	tree := t.TempDir()
	for i := range 300 {
		writeFile(t, filepath.Join(tree, fmt.Sprintf("d/f%03d", i)), "")
	}
	writeFile(t, filepath.Join(tree, "d/a"), "")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		a, b := filepath.Join(tree, "d/a"), filepath.Join(tree, "d/b")
		for {
			select {
			case <-stop:
				return
			default:
			}
			os.Rename(a, b)
			os.Rename(b, a)
		}
	}()
	defer func() { close(stop); <-done }()
	for range 30 {
		res := mustWalk(t, walkOpts(tree, newMemSink()))
		d := mustFind(t, res, "d")
		if find(res, "d/a") == nil && find(res, "d/b") == nil && d.State == StateOK && d.Listing == "" {
			t.Fatalf("the file is absent from a fully listed directory")
		}
	}
}

func TestWalkBudgetsExhaustedStayValid(t *testing.T) {
	type tc struct {
		name     string
		build    func(t *testing.T, tree string)
		set      func(o *WalkOptions)
		complete bool
		check    func(t *testing.T, res *WalkResult)
	}
	many := func(n int, dir string) func(t *testing.T, tree string) {
		return func(t *testing.T, tree string) {
			for i := range n {
				writeFile(t, filepath.Join(tree, dir, fmt.Sprintf("f%03d", i)), "data")
			}
		}
	}
	wantState := func(p string, s State, r Reason) func(t *testing.T, res *WalkResult) {
		return func(t *testing.T, res *WalkResult) {
			t.Helper()
			if p == "" {
				if res.Root.State != s || res.Root.Reason != r {
					t.Errorf("root: %+v", res.Root)
				}
				return
			}
			if e := mustFind(t, res, p); e.State != s || e.Reason != r {
				t.Errorf("%s: %+v", p, e)
			}
		}
	}
	cases := []tc{
		{name: "entries", build: many(50, "d"), set: func(o *WalkOptions) { o.Limits.Entries = 10 },
			check: wantState("d", StateTruncated, ReasonEntries)},
		{name: "manifest", build: many(50, "d"), set: func(o *WalkOptions) { o.Limits.Manifest = 10 * lineFixed },
			check: wantState("d", StateTruncated, ReasonManifest)},
		{name: "dir-entries", build: many(50, "d"), set: func(o *WalkOptions) { o.Limits.DirEntries = 5 },
			check: func(t *testing.T, res *WalkResult) {
				wantState("d", StateTruncated, ReasonDirEntries)(t, res)
				if n := strings.Count(dumpPaths(res), "d/f"); n != 5 {
					t.Errorf("%d names recorded from d", n)
				}
			}},
		{name: "depth", build: func(t *testing.T, tree string) { writeFile(t, filepath.Join(tree, "a/b/c/d/e/f"), "") },
			set: func(o *WalkOptions) { o.Limits.Depth = 3 },
			check: func(t *testing.T, res *WalkResult) {
				wantState("a/b/c", StateTruncated, ReasonDepth)(t, res)
				if find(res, "a/b/c/d") != nil {
					t.Errorf("entry below the depth limit")
				}
			}},
		{name: "path", build: func(t *testing.T, tree string) {
			writeFile(t, filepath.Join(tree, "d", strings.Repeat("n", 40)), "")
			writeFile(t, filepath.Join(tree, "d/short"), "")
		}, set: func(o *WalkOptions) { o.Limits.Path = 20 },
			check: func(t *testing.T, res *WalkResult) {
				wantState("d", StateTruncated, ReasonPath)(t, res)
				mustFind(t, res, "d/short")
			}},
		{name: "file", build: many(3, "d"), set: func(o *WalkOptions) { o.Limits.File = 3 }, complete: true,
			check: wantState("d/f000", StateTooBig, "")},
		{name: "read", build: many(3, "d"), set: func(o *WalkOptions) { o.Limits.Read = 6 },
			check: func(t *testing.T, res *WalkResult) {
				wantState("d/f000", StateOK, "")(t, res)
				wantState("d/f001", StateSkipped, ReasonRead)(t, res)
				if res.Bytes.Read > 6 {
					t.Errorf("read %d bytes over a budget of 6", res.Bytes.Read)
				}
			}},
		{name: "time", build: many(3, "d"), set: func(o *WalkOptions) { o.Deadline = time.Now().Add(-time.Second) },
			check: wantState("", StateTruncated, ReasonTime)},
		{name: "disk", build: many(3, "d"), set: func(o *WalkOptions) { o.Sink.(*memSink).lowAfter = 1 },
			check: func(t *testing.T, res *WalkResult) {
				wantState("d/f000", StateOK, "")(t, res)
				wantState("d/f001", StateSkipped, ReasonDisk)(t, res)
				wantState("d/f002", StateSkipped, ReasonDisk)(t, res)
			}},
		{name: "enospc", build: many(3, "d"), set: func(o *WalkOptions) { o.Sink.(*memSink).enospc = true },
			check: wantState("d/f002", StateSkipped, ReasonDisk)},
		{name: "escaping-heavy", build: func(t *testing.T, tree string) {
			ctl := strings.Repeat("\x01", 200)
			writeFile(t, filepath.Join(tree, "d", ctl), "")
			target := strings.Repeat("\x01\x02/", 1365)
			for i := range 20 {
				if err := os.Symlink(target, filepath.Join(tree, "d", fmt.Sprintf("l%02d", i))); err != nil {
					t.Fatal(err)
				}
			}
		}, set: func(o *WalkOptions) { o.Limits.Manifest = 200 << 10 },
			check: wantState("d", StateTruncated, ReasonManifest)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tree := t.TempDir()
			c.build(t, tree)
			o := walkOpts(tree, newMemSink())
			c.set(&o)
			res := mustWalk(t, o)
			if complete(res) != c.complete {
				t.Errorf("complete = %v: %+v %s", complete(res), res.Root, dumpPaths(res))
			}
			c.check(t, res)
		})
	}
}

func TestWalkKnownLinksCannotOverflowTheManifest(t *testing.T) {
	tree := t.TempDir()
	for i := range 30 {
		writeFile(t, filepath.Join(tree, fmt.Sprintf("d/f%02d", i)), "x")
	}
	writeFile(t, filepath.Join(tree, "e/keep"), "x")
	o := walkOpts(tree, newMemSink())
	base := baseOf(mustWalk(t, o))
	// Every known file becomes a link whose 4 KiB target is all control
	// bytes: about 24 KiB per line once escaped.
	target := strings.Repeat("\x01", MaxTargetCap-1)
	for i := range 30 {
		p := filepath.Join(tree, fmt.Sprintf("d/f%02d", i))
		os.Remove(p)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	line := maxLineBytes([]byte("d/f00"), []byte(target))
	o.Limits.Manifest = 10*line + 3*lineFixed
	o.Known, o.Cache = base, base
	res := mustWalk(t, o)
	d := mustFind(t, res, "d")
	if d.State != StateTruncated || d.Reason != ReasonManifest {
		t.Errorf("d: %+v", d)
	}
	links := strings.Count(dumpPaths(res), ":link")
	if links == 0 || links == 30 {
		t.Errorf("%d links recorded", links)
	}
	// Known paths after the grown ones are still looked up.
	if e := mustFind(t, res, "e/keep"); e.SHA256 == "" {
		t.Errorf("e/keep: %+v", e)
	}

	if complete(res) {
		t.Errorf("complete")
	}
}

func TestWalkDamagedKnownFileIsReadBeforeNewNames(t *testing.T) {
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "docs/x"), "original")
	o := walkOpts(tree, newMemSink())
	base := baseOf(mustWalk(t, o))
	writeFile(t, filepath.Join(tree, "docs/x"), "damaged by the agent")
	for i := range 2000 {
		writeFile(t, filepath.Join(tree, fmt.Sprintf("docs/flood%04d", i)), strings.Repeat("f", 100))
	}
	o.Known, o.Cache = base, base
	o.Limits.Entries = 50
	o.Limits.Read = int64(len("damaged by the agent")) + 1
	res := mustWalk(t, o)
	x := mustFind(t, res, "docs/x")
	if x.State != StateOK || x.SHA256 != sumOf([]byte("damaged by the agent")) {
		t.Errorf("docs/x: %+v", x)
	}
	if d := mustFind(t, res, "docs"); d.State != StateTruncated || d.Reason != ReasonEntries {
		t.Errorf("docs: %+v", d)
	}
	if len(res.Flood) == 0 || string(res.Flood[0].At) != "docs" {
		t.Errorf("flood: %+v", res.Flood)
	}
}

func TestWalkWorkerFloodDoesNotStarveOtherWorkers(t *testing.T) {
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "workers/a/known"), "a")
	writeFile(t, filepath.Join(tree, "workers/b/known"), "b")
	writeFile(t, filepath.Join(tree, "top"), "t")
	o := walkOpts(tree, newMemSink())
	o.WorkerDirs = [][]byte{[]byte("workers/a"), []byte("workers/b"), []byte("missing"), []byte("../x")}
	base := baseOf(mustWalk(t, o))
	for i := range 500 {
		writeFile(t, filepath.Join(tree, fmt.Sprintf("workers/a/flood%03d", i)), strings.Repeat("a", 100))
	}
	for i := range 3 {
		writeFile(t, filepath.Join(tree, fmt.Sprintf("workers/b/new%d", i)), strings.Repeat("b", 100))
	}
	o.Known, o.Cache = base, base
	o.Limits.Entries = 120
	o.Limits.Read = 1000
	res := mustWalk(t, o)
	if a := mustFind(t, res, "workers/a"); a.State != StateTruncated || a.Reason != ReasonEntries {
		t.Errorf("workers/a: %+v", a)
	}
	if b := mustFind(t, res, "workers/b"); b.State != StateOK {
		t.Errorf("workers/b: %+v", b)
	}
	for i := range 3 {
		e := mustFind(t, res, fmt.Sprintf("workers/b/new%d", i))
		if e.SHA256 != sumOf([]byte(strings.Repeat("b", 100))) {
			t.Errorf("workers/b/new%d not captured: %+v", i, e)
		}
	}
	if res.Root.State != StateOK {
		t.Errorf("root: %+v", res.Root)
	}
}

func TestWalkStatCacheAndReverify(t *testing.T) {
	resetSeams(t)
	tree := t.TempDir()
	p := filepath.Join(tree, "f")
	writeFile(t, p, "aaaa")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	sink := newMemSink()
	o := walkOpts(tree, sink)
	first := mustWalk(t, o)
	orig := *mustFind(t, first, "f")
	base := baseOf(first)

	o.Cache = base
	if res := mustWalk(t, o); res.Bytes.Read != 0 || mustFind(t, res, "f").SHA256 != orig.SHA256 {
		t.Fatalf("unchanged file not a cache hit: read %d", res.Bytes.Read)
	}

	// Rewrite the content keeping size and mtime, and forge the ctime the
	// stat sees: the cache is fooled until a re-verify reads the file.
	writeFile(t, p, "bbbb")
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	walkSeams.stat = func(rel []byte, st *sysStat) {
		if string(rel) == "f" {
			st.Ctime = orig.Ctime
		}
	}
	if res := mustWalk(t, o); mustFind(t, res, "f").SHA256 != orig.SHA256 {
		t.Fatalf("expected the forged ctime to fool the cache")
	}

	o.Reverify = 7 * 24 * time.Hour
	if res := mustWalk(t, o); mustFind(t, res, "f").SHA256 != orig.SHA256 {
		t.Fatalf("re-verified a recently verified file")
	}
	stale := baseOf(first)
	for i := range stale.Entries {
		stale.Entries[i].Verified = time.Now().Add(-8 * 24 * time.Hour).Unix()
	}
	o.Cache = stale
	res := mustWalk(t, o)
	if f := mustFind(t, res, "f"); f.SHA256 != sumOf([]byte("bbbb")) || f.State != StateOK || f.Verified < o.Start.Unix() {
		t.Errorf("stale file not re-verified: %+v", f)
	}

	o.Cache, o.Reverify, o.Full = base, 0, true
	if res := mustWalk(t, o); mustFind(t, res, "f").SHA256 != sumOf([]byte("bbbb")) {
		t.Errorf("--full did not re-read")
	}
	o.Limits.Read = 0
	if res := mustWalk(t, o); mustFind(t, res, "f").SHA256 != orig.SHA256 || !complete(res) {
		t.Errorf("re-verify ran without leftover budget, or set a state")
	}
}

func TestWalkCacheIgnoresRecentCtime(t *testing.T) {
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "f"), "x")
	o := walkOpts(tree, newMemSink())
	base := baseOf(mustWalk(t, o))
	base.Header.Created = time.Now()
	base.Header.Quiescent = QuiescentNone
	o.Cache = base
	if res := mustWalk(t, o); res.Bytes.Read == 0 {
		t.Errorf("a ctime inside the racy window was trusted")
	}
}

func TestWalkTicketIsHashedNotStored(t *testing.T) {
	tree := t.TempDir()
	ticket := `{"token":"secret"}`
	writeFile(t, filepath.Join(tree, ".lever/bootstrap.json"), ticket)
	writeFile(t, filepath.Join(tree, ".lever/other"), "o")
	writeFile(t, filepath.Join(tree, "sub/x/.lever/bootstrap.json"), "worker")
	writeFile(t, filepath.Join(tree, ".lever-files/f"), "lf")
	sink := newMemSink()
	res := mustWalk(t, walkOpts(tree, sink))
	e := mustFind(t, res, ".lever/bootstrap.json")
	if e.State != StateSecret || e.SHA256 != sumOf([]byte(ticket)) || e.Size != int64(len(ticket)) {
		t.Errorf("ticket: %+v", e)
	}
	if sink.has(e.SHA256) {
		t.Errorf("the ticket's content was stored")
	}
	if !complete(res) {
		t.Errorf("a secret entry made the snapshot incomplete")
	}
	for _, p := range []string{".lever/other", "sub/x/.lever/bootstrap.json", ".lever-files/f"} {
		e := mustFind(t, res, p)
		if e.State != StateOK || !sink.has(e.SHA256) {
			t.Errorf("%s not captured: %+v", p, e)
		}
	}

	t.Run("large", func(t *testing.T) {
		tree := t.TempDir()
		writeFile(t, filepath.Join(tree, ".lever/bootstrap.json"), strings.Repeat("x", ticketMax+1))
		res := mustWalk(t, walkOpts(tree, sink))
		if e := mustFind(t, res, ".lever/bootstrap.json"); e.State != StateOK || !sink.has(e.SHA256) {
			t.Errorf("large ticket: %+v", e)
		}
	})
	t.Run("dir", func(t *testing.T) {
		tree := t.TempDir()
		writeFile(t, filepath.Join(tree, ".lever/bootstrap.json/inner"), "i")
		res := mustWalk(t, walkOpts(tree, sink))
		if e := mustFind(t, res, ".lever/bootstrap.json/inner"); !sink.has(e.SHA256) {
			t.Errorf("inner: %+v", e)
		}
	})
	t.Run("link", func(t *testing.T) {
		tree := t.TempDir()
		os.Mkdir(filepath.Join(tree, ".lever"), 0o755)
		os.Symlink("/etc/passwd", filepath.Join(tree, ".lever/bootstrap.json"))
		res := mustWalk(t, walkOpts(tree, sink))
		if e := mustFind(t, res, ".lever/bootstrap.json"); e.Type != TypeLink || e.State != StateOK {
			t.Errorf("link: %+v", e)
		}
	})
}

func TestWalkExcludes(t *testing.T) {
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "node_modules/x"), "x")
	writeFile(t, filepath.Join(tree, "a/node_modules/y"), "y")
	writeFile(t, filepath.Join(tree, "build/a.o"), "o")
	writeFile(t, filepath.Join(tree, "build/b.c"), "c")
	writeFile(t, filepath.Join(tree, "sub/build/c.o"), "o")
	writeFile(t, filepath.Join(tree, ".gitignore"), "*\n")
	o := walkOpts(tree, newMemSink())
	o.Exclude = []string{"node_modules", "build/*.o"}
	res := mustWalk(t, o)
	for _, p := range []string{"node_modules", "a/node_modules", "build/a.o"} {
		if e := mustFind(t, res, p); e.State != StateExcluded || !e.HasMeta || e.SHA256 != "" {
			t.Errorf("%s: %+v", p, e)
		}
	}
	for _, p := range []string{"node_modules/x", "a/node_modules/y"} {
		if find(res, p) != nil {
			t.Errorf("%s: descended into an excluded directory", p)
		}
	}
	for _, p := range []string{"build/b.c", "sub/build/c.o", ".gitignore"} {
		if e := mustFind(t, res, p); e.State != StateOK || e.SHA256 == "" {
			t.Errorf("%s: %+v", p, e)
		}
	}
	if !complete(res) {
		t.Errorf("excludes made the snapshot incomplete")
	}
}

func TestWalkGuardAbortsOnStateDirectory(t *testing.T) {
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "a/state/x"), "x")
	st, err := os.Stat(filepath.Join(tree, "a/state"))
	if err != nil {
		t.Fatal(err)
	}
	sys := st.Sys().(*syscall.Stat_t)
	o := walkOpts(tree, newMemSink())
	o.Guard = []DevIno{{uint64(sys.Dev), uint64(sys.Ino)}}
	if _, err := Walk(context.Background(), o); !errors.Is(err, ErrStateInTree) || !strings.Contains(err.Error(), "a/state") {
		t.Errorf("Walk: %v", err)
	}
	o.Known = baseOf(mustWalk(t, walkOpts(tree, newMemSink())))
	if _, err := Walk(context.Background(), o); !errors.Is(err, ErrStateInTree) {
		t.Errorf("Walk with a known base: %v", err)
	}
}

func TestWalkHardLinksAreReadOnce(t *testing.T) {
	tree := t.TempDir()
	data := strings.Repeat("h", 100000)
	writeFile(t, filepath.Join(tree, "a"), data)
	if err := os.Link(filepath.Join(tree, "a"), filepath.Join(tree, "b")); err != nil {
		t.Fatal(err)
	}
	sink := newMemSink()
	res := mustWalk(t, walkOpts(tree, sink))
	a, b := mustFind(t, res, "a"), mustFind(t, res, "b")
	if a.SHA256 != sumOf([]byte(data)) || b.SHA256 != a.SHA256 || a.Nlink != 2 {
		t.Errorf("a %+v b %+v", a, b)
	}
	if res.Bytes.Read != int64(len(data)) || sink.writes != 1 {
		t.Errorf("read %d bytes in %d blobs", res.Bytes.Read, sink.writes)
	}
}

func TestWalkSparseFileCostsItsApparentSize(t *testing.T) {
	tree := t.TempDir()
	f, err := os.Create(filepath.Join(tree, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(8 << 20)
	f.Close()
	o := walkOpts(tree, newMemSink())
	res := mustWalk(t, o)
	if e := mustFind(t, res, "sparse"); e.Size != 8<<20 || res.Bytes.Read != 8<<20 {
		t.Errorf("sparse: %+v, read %d", e, res.Bytes.Read)
	}
	o.Limits.Read = 4 << 20
	res = mustWalk(t, o)
	if e := mustFind(t, res, "sparse"); e.State != StateSkipped || e.Reason != ReasonRead || res.Bytes.Read != 0 {
		t.Errorf("sparse over the read budget: %+v, read %d", e, res.Bytes.Read)
	}
}

func TestWalkInterrupt(t *testing.T) {
	t.Run("pass1", func(t *testing.T) {
		resetSeams(t)
		tree := t.TempDir()
		writeFile(t, filepath.Join(tree, "a/f"), "a")
		writeFile(t, filepath.Join(tree, "b/f"), "b")
		intr := make(chan struct{})
		walkSeams.beforeList = func(rel []byte) {
			if string(rel) == "a" {
				close(intr)
			}
		}
		o := walkOpts(tree, newMemSink())
		o.Interrupt = intr
		res := mustWalk(t, o)
		for _, p := range []string{"a", "b"} {
			if e := mustFind(t, res, p); e.State != StateTruncated || e.Reason != ReasonInterrupted {
				t.Errorf("%s: %+v", p, e)
			}
		}
		if res.Root.State != StateOK {
			t.Errorf("root was fully listed: %+v", res.Root)
		}
	})
	t.Run("pass2", func(t *testing.T) {
		resetSeams(t)
		tree := t.TempDir()
		writeFile(t, filepath.Join(tree, "a"), strings.Repeat("a", 3<<20))
		writeFile(t, filepath.Join(tree, "b"), strings.Repeat("b", 4<<20))
		intr := make(chan struct{})
		walkSeams.afterChunk = func(rel []byte, _ int, off int64) {
			if string(rel) == "a" && off == 1<<20 {
				close(intr)
			}
		}
		o := walkOpts(tree, newMemSink())
		o.Interrupt = intr
		res := mustWalk(t, o)
		for _, p := range []string{"a", "b"} {
			if e := mustFind(t, res, p); e.State != StateSkipped || e.Reason != ReasonInterrupted {
				t.Errorf("%s: %+v", p, e)
			}
		}
	})
	t.Run("cancel", func(t *testing.T) {
		tree := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Walk(ctx, walkOpts(tree, newMemSink())); !errors.Is(err, context.Canceled) {
			t.Errorf("Walk: %v", err)
		}
	})
}

func TestWalkBadAndDuplicateNamesAreCounted(t *testing.T) {
	resetSeams(t)
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "f"), "f")
	walkSeams.listed = func(rel []byte, chunk int, names []string) []string {
		if len(rel) == 0 && chunk == 0 {
			return append(names, "", ".", "..", "a/b", "x\x00y", "f")
		}
		return names
	}
	res := mustWalk(t, walkOpts(tree, newMemSink()))
	if res.Badname != 6 || len(res.Entries) != 1 {
		t.Errorf("badname %d, entries %s", res.Badname, dumpPaths(res))
	}
}

func TestWalkSwappedParentIsNotReadInPass2(t *testing.T) {
	resetSeams(t)
	tree := t.TempDir()
	for i := range 100 {
		writeFile(t, filepath.Join(tree, fmt.Sprintf("d%03d/f", i)), strings.Repeat("x", 200-i))
	}
	walkSeams.beforeOpen = func(rel []byte) {
		if string(rel) != "d099/f" {
			return
		}
		// d000's fd is long evicted; the reopen must notice the swap.
		os.Rename(filepath.Join(tree, "d000"), filepath.Join(tree, "old"))
		writeFile(t, filepath.Join(tree, "d000/f"), "planted")
	}
	res := mustWalk(t, walkOpts(tree, newMemSink()))
	if e := mustFind(t, res, "d000/f"); e.State != StateChanged || e.SHA256 != "" {
		t.Errorf("d000/f: %+v", e)
	}
	if e := mustFind(t, res, "d050/f"); e.SHA256 == "" {
		t.Errorf("d050/f: %+v", e)
	}
}

func TestWalkKnownBaseDeepAndWideTrees(t *testing.T) {
	tree := t.TempDir()
	deep := tree
	for i := range 100 {
		deep = filepath.Join(deep, fmt.Sprintf("l%d", i))
	}
	writeFile(t, filepath.Join(deep, "leaf"), "leaf")
	for i := range 150 {
		writeFile(t, filepath.Join(tree, fmt.Sprintf("w/d%03d/f", i)), fmt.Sprint(i))
	}
	sink := newMemSink()
	o := walkOpts(tree, sink)
	first := mustWalk(t, o)
	if !complete(first) {
		t.Fatalf("first walk incomplete")
	}
	o.Known = baseOf(first)
	second := mustWalk(t, o)
	if len(second.Entries) != len(first.Entries) || !complete(second) {
		t.Fatalf("second walk: %d entries, complete %v", len(second.Entries), complete(second))
	}
	for i := range first.Entries {
		if first.Entries[i].SHA256 != second.Entries[i].SHA256 {
			t.Errorf("%s differs", Q(first.Entries[i].Path))
		}
	}
}

func TestBaseStreamsAndStopsAtAnError(t *testing.T) {
	h := &Header{ID: "x"}
	es := []Entry{{Path: []byte("b"), Type: TypeFile}, {Path: []byte("a"), Type: TypeDir}}
	var got []string
	(&Base{Header: h, Open: memManifest(h, es)}).each(func(e *Entry) bool {
		got = append(got, string(e.Path))
		return true
	})
	if strings.Join(got, ",") != "a,b" {
		t.Fatalf("streamed %q", got)
	}
	got = nil
	bad := func() (EntryReader, error) { return &memReader{h: h, entries: es[:1], err: ErrInvalid}, nil }
	(&Base{Header: h, Open: bad}).each(func(e *Entry) bool {
		got = append(got, string(e.Path))
		return true
	})
	if len(got) != 1 {
		t.Fatalf("streamed %q past an error", got)
	}
}

// refEncodeString is the manifest's string encoding as the contract states
// it, to check maxStringBytes against.
func refEncodeString(b []byte) string {
	if !utf8.Valid(b) {
		return `"` + strings.Repeat("A", (len(b)+2)/3*4) + `"`
	}
	var s strings.Builder
	s.WriteByte('"')
	for _, r := range string(b) {
		switch {
		case r == '"' || r == '\\':
			s.WriteByte('\\')
			s.WriteRune(r)
		case r < 0x20:
			fmt.Fprintf(&s, `\u%04x`, r)
		case r == 0x2028 || r == 0x2029:
			fmt.Fprintf(&s, `\u%04x`, r)
		default:
			s.WriteRune(r)
		}
	}
	s.WriteByte('"')
	return s.String()
}

func TestMaxLineBytesBoundsTheEncoding(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	alphabet := []string{"a", "\x01", "\"", "\\", " ", "é", "\xff", "/", "<", "\x7f", "日"}
	gen := func() []byte {
		var b []byte
		for range r.IntN(300) {
			b = append(b, alphabet[r.IntN(len(alphabet))]...)
		}
		return b
	}
	for range 2000 {
		p, tg := gen(), gen()
		// Quotes are inside lineFixed's empty strings.
		need := int64(len(refEncodeString(p))-2) + int64(len(refEncodeString(tg))-2)
		if got := maxStringBytes(p) + maxStringBytes(tg); got < need {
			t.Fatalf("bound %d < encoded %d for %q / %q", got, need, p, tg)
		}
	}
}

// TestWalkPerformance measures a cold and a cached walk of 100k files. It
// runs only with LEVER_TREEBACKUP_PERF set to a directory to build the tree
// in.
func TestWalkPerformance(t *testing.T) {
	dir := os.Getenv("LEVER_TREEBACKUP_PERF")
	if dir == "" {
		t.Skip("LEVER_TREEBACKUP_PERF not set")
	}
	tree, err := os.MkdirTemp(dir, "perf")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tree)
	for d := range 1000 {
		os.MkdirAll(filepath.Join(tree, fmt.Sprintf("d%03d", d)), 0o755)
		for f := range 100 {
			os.WriteFile(filepath.Join(tree, fmt.Sprintf("d%03d/f%03d", d, f)), []byte(fmt.Sprintf("%d/%d", d, f)), 0o644)
		}
	}
	sink := newMemSink()
	o := walkOpts(tree, sink)
	start := time.Now()
	first := mustWalk(t, o)
	cold := time.Since(start)
	o.Known, o.Cache = baseOf(first), baseOf(first)
	start = time.Now()
	second := mustWalk(t, o)
	cached := time.Since(start)
	t.Logf("%d entries: cold %v (read %d bytes), cached %v (read %d bytes)", len(first.Entries), cold, first.Bytes.Read, cached, second.Bytes.Read)
}

func TestWalkDirEntriesCutDirectoryStillReadsKnownFiles(t *testing.T) {
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "docs/x"), "original")
	deep := filepath.Join(tree, "e")
	for range 80 {
		deep = filepath.Join(deep, "d")
	}
	writeFile(t, filepath.Join(deep, "f"), "deep")
	o := walkOpts(tree, newMemSink())
	base := baseOf(mustWalk(t, o))
	writeFile(t, filepath.Join(tree, "docs/x"), "damaged")
	for i := range 50 {
		writeFile(t, filepath.Join(tree, fmt.Sprintf("docs/flood%02d", i)), "f")
	}
	o.Known, o.Cache = base, base
	o.Limits.DirEntries = 20
	res := mustWalk(t, o)
	if d := mustFind(t, res, "docs"); d.State != StateTruncated || d.Reason != ReasonDirEntries {
		t.Errorf("docs: %+v", d)
	}
	if x := mustFind(t, res, "docs/x"); x.SHA256 != sumOf([]byte("damaged")) {
		t.Errorf("docs/x: %+v", x)
	}
	for _, e := range res.Entries {
		if IsUnder(e.Path, []byte("docs")) && e.Type == TypeFile && e.SHA256 == "" {
			t.Errorf("%s not read: %+v", Q(e.Path), e)
		}
	}
}

func TestWalkFileSwappedForLinkBeforeReadIsNotFollowed(t *testing.T) {
	resetSeams(t)
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "f"), "content")
	walkSeams.beforeOpen = func(rel []byte) {
		if string(rel) == "f" {
			os.Remove(filepath.Join(tree, "f"))
			os.Symlink("/etc/passwd", filepath.Join(tree, "f"))
		}
	}
	sink := newMemSink()
	res := mustWalk(t, walkOpts(tree, sink))
	if e := mustFind(t, res, "f"); e.State != StateChanged || e.SHA256 != "" {
		t.Errorf("f: %+v", e)
	}
	if len(sink.blobs) != 0 {
		t.Errorf("%d blobs stored", len(sink.blobs))
	}
}

func TestWalkKnownDirSwappedBeforeListingIsChanged(t *testing.T) {
	resetSeams(t)
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "a/inner"), "a")
	o := walkOpts(tree, newMemSink())
	o.Known = baseOf(mustWalk(t, o))
	walkSeams.beforeList = func(rel []byte) {
		if len(rel) == 0 {
			os.Rename(filepath.Join(tree, "a"), filepath.Join(tree, "a.old"))
			writeFile(t, filepath.Join(tree, "a/planted"), "p")
		}
	}
	res := mustWalk(t, o)
	if e := mustFind(t, res, "a"); e.State != StateChanged {
		t.Errorf("a: %+v", e)
	}
	if find(res, "a/planted") != nil {
		t.Errorf("listed the directory swapped in")
	}
}

func TestWalkGuardOnTheRootAborts(t *testing.T) {
	tree := t.TempDir()
	var st syscall.Stat_t
	if err := syscall.Stat(tree, &st); err != nil {
		t.Fatal(err)
	}
	o := walkOpts(tree, newMemSink())
	o.Guard = []DevIno{{uint64(st.Dev), uint64(st.Ino)}}
	if _, err := Walk(context.Background(), o); !errors.Is(err, ErrStateInTree) {
		t.Errorf("Walk: %v", err)
	}
}

func TestWalkOtherDeviceIsMountWhereDevIsChecked(t *testing.T) {
	if !platformChecksDev {
		t.Skip("this platform detects mounts with RESOLVE_NO_XDEV")
	}
	resetSeams(t)
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "m/inner"), "i")
	walkSeams.stat = func(rel []byte, st *sysStat) {
		if string(rel) == "m" {
			st.Dev++
		}
	}
	res := mustWalk(t, walkOpts(tree, newMemSink()))
	if e := mustFind(t, res, "m"); e.State != StateMount {
		t.Errorf("m: %+v", e)
	}
}

func TestWalkRecentDirectoryIsListingUnstable(t *testing.T) {
	saved := listingWindow
	listingWindow = time.Hour
	t.Cleanup(func() { listingWindow = saved })
	tree := t.TempDir()
	writeFile(t, filepath.Join(tree, "f"), "f")
	res := mustWalk(t, walkOpts(tree, newMemSink()))
	if res.Root.State != StateListingUnstable {
		t.Errorf("root: %+v", res.Root)
	}
}
