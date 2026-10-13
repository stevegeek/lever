package treebackup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"
)

// memBlobs is a BlobSource over a map from digest to content.
type memBlobs map[string][]byte

func (m memBlobs) OpenBlob(sum string) (io.ReadCloser, int64, error) {
	b, ok := m[sum]
	if !ok {
		return nil, 0, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func hdr(id string) *Header {
	return &Header{Version: 1, ID: id, Seq: 1, Trigger: TriggerManual, Quiescent: QuiescentNone, Complete: true}
}

func dirE(p string) Entry {
	return Entry{Path: []byte(p), Type: TypeDir, HasMeta: true, Mode: 0o755, Mtime: 1}
}

// fileE makes a captured file and stores its content in blobs (when not
// nil).
func fileE(blobs memBlobs, p, content string) Entry {
	if blobs != nil {
		blobs[sumOf([]byte(content))] = []byte(content)
	}
	return Entry{Path: []byte(p), Type: TypeFile, HasMeta: true, Mode: 0o644, Size: int64(len(content)),
		Mtime: 1, Nlink: 1, SHA256: sumOf([]byte(content)), Verified: 1}
}

func linkE(p, target string) Entry {
	return Entry{Path: []byte(p), Type: TypeLink, HasMeta: true, Mode: 0o777, Mtime: 1, Target: []byte(target), HasTarget: true}
}

func with(e Entry, f func(*Entry)) Entry { f(&e); return e }

// badManifest yields its entries and then a validation error.
func badManifest(h *Header, entries []Entry) OpenManifest {
	es := slices.Clone(entries)
	slices.SortFunc(es, func(a, b Entry) int { return ComparePaths(a.Path, b.Path) })
	return func() (EntryReader, error) {
		return &memReader{h: h, entries: es, err: fmt.Errorf("%w: duplicate key", ErrInvalid)}, nil
	}
}

func runDiff(t *testing.T, oldM, newM OpenManifest, o DiffOptions) (string, DiffCounts) {
	t.Helper()
	var b bytes.Buffer
	c, err := Diff(&b, oldM, newM, o)
	if err != nil {
		t.Fatalf("Diff: %v\n%s", err, b.String())
	}
	return b.String(), c
}

// summaryLines returns the "<status> <path>..." lines of a diff.
func summaryLines(out string) []string {
	var r []string
	for _, l := range strings.Split(out, "\n") {
		if len(l) > 2 && l[1] == ' ' && strings.ContainsRune("ADMTLP?~", rune(l[0])) {
			r = append(r, l)
		}
	}
	return r
}

func hasLine(out, prefix string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func TestDiffStatuses(t *testing.T) {
	b := memBlobs{}
	old := []Entry{
		dirE("a"), fileE(b, "a/f", "one\n"), fileE(b, "a/keep", "same\n"),
		dirE("gone"), fileE(b, "gone/x", "x"), fileE(b, "gone/y", "y"), dirE("gone/sub"), fileE(b, "gone/sub/z", "z"),
		dirE("sub"), dirE("sub/x"), fileE(b, "sub/x/secret", "s"),
		linkE("l", "t1"), fileE(b, "mode", "m"), fileE(b, "touched", "t"),
		fileE(b, "f2link", "f"),
	}
	nw := []Entry{
		dirE("a"), fileE(b, "a/f", "two\n"), fileE(b, "a/keep", "same\n"),
		dirE("new"), fileE(b, "new/n1", "n1"), fileE(b, "new/n2", "n2"),
		linkE("sub", "/home/op"),
		linkE("l", "t2"), with(fileE(b, "mode", "m"), func(e *Entry) { e.Mode = 0o4755 }),
		with(fileE(b, "touched", "t"), func(e *Entry) { e.Mtime = 99 }),
		linkE("f2link", "f"),
	}
	out, c := runDiff(t, memManifest(hdr("o"), old), memManifest(hdr("n"), nw), DiffOptions{Blobs: b})
	want := []string{
		"M a/f  (size 4 → 4, sha256 " + shortHash(sumOf([]byte("one\n"))) + " → " + shortHash(sumOf([]byte("two\n"))) + ")",
		"T f2link  (file → link)",
		"D gone  (dir, 4 entries below)",
		"L l  (t1 → t2)",
		"P mode  (mode 0644 → 4755)",
		"A new  (dir, 2 entries below)",
		"T sub  (dir → link, 2 entries below)",
	}
	if got := summaryLines(out); !slices.Equal(got, want) {
		t.Fatalf("summary:\n%s\nwant:\n%s\nfull output:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"), out)
	}
	if c != (DiffCounts{A: 3, D: 7, M: 1, T: 2, L: 1, P: 1}) {
		t.Fatalf("counts %+v", c)
	}
	if !strings.Contains(out, "== M a/f\n@@ -1,1 +1,1 @@\n- one\n+ two\n") {
		t.Fatalf("no text section:\n%s", out)
	}
	if strings.Contains(out, "touched") {
		t.Fatal("an mtime-only change shows without Meta")
	}
	out, c = runDiff(t, memManifest(hdr("o"), old), memManifest(hdr("n"), nw), DiffOptions{Meta: true})
	if !hasLine(out, "~ touched  (mtime ") || c.Meta != 1 {
		t.Fatalf("Meta: %+v\n%s", c, out)
	}
}

func TestDiffGolden(t *testing.T) {
	b := memBlobs{}
	old := []Entry{fileE(b, "notes.txt", "line 1\nline 2\nline 3\n"), fileE(b, "x\xff", "a")}
	nw := []Entry{
		fileE(b, "notes.txt", "line 1\n\x1b]0;pwned\x07\n--- a/etc/passwd\n== M fake\n@@ -1 +1 @@\nline 3"),
		fileE(b, "x\xfe", "a"),
	}
	out, _ := runDiff(t, memManifest(hdr("20260101T000000Z"), old), memManifest(hdr("20260102T000000Z"), nw), DiffOptions{Blobs: b})
	want := `old 20260101T000000Z (complete)
new 20260102T000000Z (complete)
changes: A 1, D 1, M 1, T 0, L 0, P 0, ? 0
M notes.txt  (size 21 → 63, sha256 ` + shortHash(old[0].SHA256) + ` → ` + shortHash(nw[0].SHA256) + `)
A "x\xfe"  (file, size 1, sha256 ` + shortHash(sumOf([]byte("a"))) + `)
D "x\xff"  (file, size 1, sha256 ` + shortHash(sumOf([]byte("a"))) + `)
== M notes.txt
@@ -1,3 +1,6 @@
  line 1
- line 2
- line 3
+\\x1b]0;pwned\x07
+ --- a/etc/passwd
+ == M fake
+ @@ -1 +1 @@
+ line 3
\ no newline at end of file
`
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	// No line outside the lever-made headers starts with '=' or '@'.
	for _, l := range strings.Split(out, "\n") {
		if (strings.HasPrefix(l, "=") && !strings.HasPrefix(l, "== M ")) || (strings.HasPrefix(l, "@") && !strings.HasPrefix(l, "@@ -")) {
			t.Errorf("forged header line %q", l)
		}
	}
}

func TestDiffCompleteVsTruncatedShowsNoDeletions(t *testing.T) {
	var old, nw []Entry
	for i := range 50 {
		old = append(old, fileE(nil, fmt.Sprintf("f%02d", i), "c"))
	}
	old = append(old, dirE("d"), fileE(nil, "d/1", "1"), fileE(nil, "d/2", "2"))
	nw = append(nw, old[0], old[50], old[51])
	trunc := hdr("n")
	trunc.Complete = false
	trunc.Root.State = StateTruncated
	trunc.Root.Reason = ReasonEntries
	out, c := runDiff(t, memManifest(hdr("o"), old), memManifest(trunc, nw), DiffOptions{})
	if c.D != 0 || c.Unknown != 50 {
		t.Fatalf("counts %+v\n%s", c, out)
	}
	if !hasLine(out, "? f01  (not seen in new: the tree root is truncated (entries))") {
		t.Fatalf("no per-name ?:\n%s", out)
	}
	if !hasLine(out, "? d/2  (not seen in new: the tree root is truncated (entries))") {
		t.Fatalf("names below a present dir are ? too:\n%s", out)
	}

	// A truncated directory hides only its own subtree.
	trunc2 := hdr("n2")
	nw2 := []Entry{with(dirE("d"), func(e *Entry) { e.State, e.Reason = StateTruncated, ReasonDirEntries }), old[51]}
	nw2 = append(nw2, old[:50]...)
	out, c = runDiff(t, memManifest(hdr("o"), old), memManifest(trunc2, nw2), DiffOptions{})
	if c.D != 0 || c.Unknown != 2 || !hasLine(out, "? d/2  (not seen in new: d truncated (dir-entries))") {
		t.Fatalf("counts %+v\n%s", c, out)
	}

	// And the other way: names new since an incomplete snapshot are ?.
	out, c = runDiff(t, memManifest(trunc, nw), memManifest(hdr("o"), old), DiffOptions{})
	if c.A != 0 || c.Unknown != 50 {
		t.Fatalf("reverse counts %+v\n%s", c, out)
	}
}

func TestDiffListingUnstableAbsencesAreUnknown(t *testing.T) {
	old := []Entry{dirE("d"), fileE(nil, "d/a", "a"), fileE(nil, "d/b", "b")}
	nw := []Entry{with(dirE("d"), func(e *Entry) { e.State = StateListingUnstable }), fileE(nil, "d/a", "a")}
	out, c := runDiff(t, memManifest(hdr("o"), old), memManifest(hdr("n"), nw), DiffOptions{})
	if c.D != 0 || c.Unknown != 2 {
		t.Fatalf("counts %+v\n%s", c, out)
	}
	if !hasLine(out, "? d/b  (not seen in new: d listing-unstable)") {
		t.Fatalf("output:\n%s", out)
	}

	// A root that is listing-unstable makes top-level absences unknown.
	h := hdr("n")
	h.Root.State = StateListingUnstable
	out, c = runDiff(t, memManifest(hdr("o"), old), memManifest(h, nil), DiffOptions{})
	if c.D != 0 || c.Unknown != 3 || !hasLine(out, "? d  (not seen in new: the tree root is listing-unstable)") {
		t.Fatalf("root: %+v\n%s", c, out)
	}
}

func TestDiffHiddenByPermissionsSaysBecameUnreadable(t *testing.T) {
	old := []Entry{
		dirE("d000"), fileE(nil, "d000/x", "x"),
		dirE("d0300"), fileE(nil, "d0300/known", "k"), fileE(nil, "d0300/other", "o"),
		dirE("d0600"), fileE(nil, "d0600/y", "y"),
		fileE(nil, "f000", "f"),
	}
	nw := []Entry{
		with(dirE("d000"), func(e *Entry) { e.Mode, e.State = 0, StateUnreadable }),
		with(dirE("d0300"), func(e *Entry) { e.Mode, e.Listing = 0o300, ListingUnreadable }),
		fileE(nil, "d0300/known", "k"),
		with(dirE("d0600"), func(e *Entry) { e.Mode = 0o600 }),
		{Path: []byte("d0600/y"), Type: TypeUnknown, State: StateUnreadable},
		with(fileE(nil, "f000", "f"), func(e *Entry) { e.Mode, e.State, e.SHA256, e.Verified = 0, StateUnreadable, "", 0 }),
	}
	out, c := runDiff(t, memManifest(hdr("o"), old), memManifest(hdr("n"), nw), DiffOptions{})
	for _, w := range []string{
		"? d000  (listing not captured: became unreadable, mode 0755 → 0000)",
		"? d000/x  (not seen in new: d000 became unreadable)",
		"? d0300  (listing not captured: listing became unreadable, mode 0755 → 0300)",
		"? d0300/other  (not seen in new: d0300 listing became unreadable)",
		"P d0600  (mode 0755 → 0600)",
		"? d0600/y  (type file → unknown (unreadable), became unreadable)",
		"? f000  (became unreadable, mode 0644 → 0000)",
	} {
		if !hasLine(out, w) {
			t.Errorf("missing %q", w)
		}
	}
	if c.D != 0 || c.T != 0 || c.M != 0 {
		t.Errorf("counts %+v", c)
	}
	if t.Failed() {
		t.Log(out)
	}
}

func TestDiffContentUnknownCases(t *testing.T) {
	old := []Entry{
		fileE(nil, "big", "small"),
		linkE("lnk", "t"),
		fileE(nil, "tounknown", "u"),
		dirE("dir2unknown"), fileE(nil, "dir2unknown/c", "c"),
		with(fileE(nil, "toobig", "x"), func(e *Entry) { e.State, e.SHA256, e.Verified, e.Size = StateTooBig, "", 0, 9<<30 }),
	}
	nw := []Entry{
		with(fileE(nil, "big", "x"), func(e *Entry) { e.State, e.SHA256, e.Verified, e.Size = StateTooBig, "", 0, 1<<30 }),
		with(linkE("lnk", ""), func(e *Entry) { e.Target, e.HasTarget = nil, false }),
		{Path: []byte("tounknown"), Type: TypeUnknown, State: StateUnreadable},
		{Path: []byte("dir2unknown"), Type: TypeUnknown, State: StateUnreadable},
		with(fileE(nil, "toobig", "x"), func(e *Entry) { e.State, e.SHA256, e.Verified, e.Size = StateTooBig, "", 0, 9<<30 }),
	}
	out, c := runDiff(t, memManifest(hdr("o"), old), memManifest(hdr("n"), nw), DiffOptions{})
	for _, w := range []string{
		"? big  (state ok → too-big)",
		"? lnk  (link target became unreadable)",
		"? tounknown  (type file → unknown (unreadable), became unreadable)",
		"? dir2unknown  (type dir → unknown (unreadable), became unreadable)",
		"? dir2unknown/c  (not seen in new: dir2unknown is unknown (unreadable))",
	} {
		if !hasLine(out, w) {
			t.Errorf("missing %q", w)
		}
	}
	if c.T != 0 || c.D != 0 || c.L != 0 || c.Unknown != 5 {
		t.Errorf("counts %+v", c)
	}
	if strings.Contains(out, "toobig") {
		t.Error("an unchanged too-big file shows")
	}
	if t.Failed() {
		t.Log(out)
	}
}

func TestDiffSecretTicket(t *testing.T) {
	sec := func(content string) Entry {
		return with(fileE(nil, ".lever/bootstrap.json", content), func(e *Entry) { e.State = StateSecret })
	}
	b := memBlobs{}
	old := []Entry{dirE(".lever"), sec("ticket-1")}
	nw := []Entry{dirE(".lever"), sec("ticket-2")}
	out, c := runDiff(t, memManifest(hdr("o"), old), memManifest(hdr("n"), nw), DiffOptions{Blobs: b})
	if c.M != 1 || !hasLine(out, "M .lever/bootstrap.json  secret (size 8 → 8, sha256 ") || strings.Contains(out, "==") {
		t.Fatalf("%+v\n%s", c, out)
	}
}

func TestDiffBinaryAndDamagedBlobs(t *testing.T) {
	b := memBlobs{}
	old := []Entry{fileE(b, "bin", "a\x00b"), fileE(b, "huge", strings.Repeat("x", diffTextMax+1)), fileE(b, "lost", "1")}
	nw := []Entry{fileE(b, "bin", "a\x00c"), fileE(b, "huge", strings.Repeat("y", diffTextMax+1)), fileE(nil, "lost", "2")}
	var out bytes.Buffer
	_, err := Diff(&out, memManifest(hdr("o"), old), memManifest(hdr("n"), nw), DiffOptions{Blobs: b})
	if err == nil || !strings.Contains(err.Error(), "run lever tree verify") {
		t.Fatalf("err %v", err)
	}
	s := out.String()
	if !hasLine(s, "M bin  binary (size 3 → 3, sha256 ") || !hasLine(s, "M huge  binary (size 1048577 → 1048577") ||
		!hasLine(s, "M lost  content unavailable: blob ") || strings.Contains(s, "==") {
		t.Fatalf("output:\n%s", s)
	}
}

func TestDiffAbsentUnknownCap(t *testing.T) {
	var old []Entry
	for i := range 1500 {
		old = append(old, fileE(nil, fmt.Sprintf("f%04d", i), "c"))
	}
	h := hdr("n")
	h.Root.Listing = ListingUnreadable
	out, c := runDiff(t, memManifest(hdr("o"), old), memManifest(h, nil), DiffOptions{FullDiff: true})
	if c.Unknown != 1500 || len(summaryLines(out)) != 1001 {
		t.Fatalf("%+v, %d lines", c, len(summaryLines(out)))
	}
	if !hasLine(out, "? … 500 more names not seen on one side") {
		t.Fatalf("no count line:\n%s", out[len(out)-300:])
	}
}

func TestDiffPathsFilter(t *testing.T) {
	old := []Entry{dirE("a"), dirE("a/b"), fileE(nil, "a/b/c", "1"), fileE(nil, "a/b/d", "2"), fileE(nil, "z", "z")}
	nw := []Entry{fileE(nil, "z", "zz")}
	out, c := runDiff(t, memManifest(hdr("o"), old), memManifest(hdr("n"), nw), DiffOptions{Paths: [][]byte{[]byte("a/b")}})
	want := []string{"D a/b  (dir, under a (D), 2 entries below)"}
	if got := summaryLines(out); !slices.Equal(got, want) || c != (DiffCounts{D: 3}) {
		t.Fatalf("%+v %q\n%s", c, got, out)
	}
	out, c = runDiff(t, memManifest(hdr("o"), old), memManifest(hdr("n"), nw), DiffOptions{Paths: [][]byte{[]byte("z"), []byte("a/b/d")}})
	want = []string{"D a/b/d  (file, size 1, sha256 " + shortHash(sumOf([]byte("2"))) + ", under a (D))", "M z  (size 1 → 2, sha256 " + shortHash(sumOf([]byte("z"))) + " → " + shortHash(sumOf([]byte("zz"))) + ")"}
	if got := summaryLines(out); !slices.Equal(got, want) || c != (DiffCounts{D: 1, M: 1}) {
		t.Fatalf("%+v %q\n%s", c, got, out)
	}
}

func TestDiffInvalidManifestPrintsNothing(t *testing.T) {
	good := memManifest(hdr("o"), []Entry{fileE(nil, "a", "1")})
	bad := badManifest(hdr("n"), []Entry{fileE(nil, "a", "2"), fileE(nil, "b", "evil")})
	for i, pair := range [][2]OpenManifest{{good, bad}, {bad, good}} {
		var out bytes.Buffer
		_, err := Diff(&out, pair[0], pair[1], DiffOptions{})
		if !errors.Is(err, ErrInvalid) || out.Len() != 0 {
			t.Errorf("case %d: err %v, output %q", i, err, out.String())
		}
	}
}

func TestDiffManyChangesDefaultsToStat(t *testing.T) {
	b := memBlobs{}
	var old, nw []Entry
	for i := range DiffStatThreshold + 1 {
		p := fmt.Sprintf("f%04d", i)
		old = append(old, fileE(b, p, "old\n"))
		nw = append(nw, fileE(b, p, fmt.Sprintf("new %d\n", i)))
	}
	out, _ := runDiff(t, memManifest(hdr("o"), old), memManifest(hdr("n"), nw), DiffOptions{Blobs: b})
	if strings.Contains(out, "== M") || !strings.Contains(out, "--full-diff") {
		t.Fatal("no stat default")
	}
	out, _ = runDiff(t, memManifest(hdr("o"), old), memManifest(hdr("n"), nw), DiffOptions{Blobs: b, FullDiff: true})
	if strings.Count(out, "\n== M ") != DiffStatThreshold+1 {
		t.Fatal("FullDiff did not print sections")
	}
	out, _ = runDiff(t, memManifest(hdr("o"), old[:3]), memManifest(hdr("n"), nw[:3]), DiffOptions{Blobs: b, Stat: true})
	if strings.Contains(out, "== M") || len(summaryLines(out)) != 3 {
		t.Fatal("Stat printed sections")
	}
}

func TestDiffOutputLimit(t *testing.T) {
	var nw []Entry
	long := strings.Repeat("n", 200)
	for i := range 60000 {
		nw = append(nw, fileE(nil, fmt.Sprintf("%s%06d", long, i), "c"))
	}
	var out bytes.Buffer
	_, err := Diff(&out, memManifest(hdr("o"), nil), memManifest(hdr("n"), nw), DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Len() > DiffOutputLimit || !strings.HasSuffix(out.String(), outLimitMarker+"\n") {
		t.Fatalf("output %d bytes, tail %q", out.Len(), out.String()[out.Len()-80:])
	}
	out.Reset()
	if _, err := Diff(&out, memManifest(hdr("o"), nil), memManifest(hdr("n"), nw), DiffOptions{NoLimit: true}); err != nil {
		t.Fatal(err)
	}
	if out.Len() <= DiffOutputLimit || strings.Contains(out.String(), outLimitMarker) {
		t.Fatalf("NoLimit output %d bytes", out.Len())
	}
}

func TestDiffPathologicalPairIsFast(t *testing.T) {
	b := memBlobs{}
	var x, y strings.Builder
	for i := range 100_000 {
		x.WriteString("a\n")
		fmt.Fprintf(&y, "%d\n", (i*7919)%100_000)
	}
	old := []Entry{fileE(b, "f", x.String())}
	nw := []Entry{fileE(b, "f", y.String())}
	var out byteCounter
	start := time.Now()
	c, err := Diff(&out, memManifest(hdr("o"), old), memManifest(hdr("n"), nw), DiffOptions{Blobs: b, NoLimit: true})
	if err != nil || c.M != 1 || out.n < 100_000 {
		t.Fatalf("%v %+v %d", err, c, out.n)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
}

type byteCounter struct{ n int64 }

func (c *byteCounter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }
