package treebackup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const testID = "20261013T120000Z"

func testHeader(id string) *Header {
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 123456789, time.UTC)
	return &Header{
		Version:      ManifestVersion,
		ID:           id,
		Seq:          1,
		Created:      t0,
		Finished:     t0.Add(3 * time.Second),
		Trigger:      TriggerStop,
		Quiescent:    QuiescentVMOff,
		LeverVersion: "0.38.0",
		Tree:         []byte("/home/u/inst/tree"),
		TreeDev:      66,
		TreeIno:      1234,
		Root:         RootInfo{HasMeta: true, Mode: 0o755, Mtime: 1},
		Limits:       DefaultLimits(),
	}
}

func sumOfString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func mDirE(p string) Entry {
	return Entry{Path: []byte(p), Type: TypeDir, HasMeta: true, Mode: 0o755, Mtime: 5, Ctime: 6, Dev: 1, Ino: 2, Nlink: 2}
}

func mFileE(p, content string) Entry {
	return Entry{Path: []byte(p), Type: TypeFile, HasMeta: true, Mode: 0o644, Size: int64(len(content)), Mtime: 5, Ctime: 6,
		Dev: 1, Ino: 3, Nlink: 1, SHA256: sumOfString(content), Verified: 100}
}

func mLinkE(p, target string) Entry {
	return Entry{Path: []byte(p), Type: TypeLink, HasMeta: true, Mode: 0o777, Mtime: 5, Ctime: 6, Dev: 1, Ino: 4, Nlink: 1,
		Target: []byte(target), HasTarget: true}
}

// encode writes a manifest with h's derived fields filled from entries.
func encode(t *testing.T, h *Header, entries []Entry) []byte {
	t.Helper()
	Summarize(h, entries)
	var b bytes.Buffer
	if err := WriteManifest(&b, h, entries); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// encodeRaw writes entries without deriving anything, so the header can
// disagree with them.
func encodeRaw(t *testing.T, h *Header, entries []Entry) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := WriteManifest(&b, h, entries); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// retrailer recomputes the trailer of a manifest whose lines were edited.
func retrailer(b []byte) []byte {
	i := bytes.LastIndex(b[:len(b)-1], []byte("\n"))
	body := b[:i+1]
	sum := sha256.Sum256(body)
	out := slices.Clone(body)
	out = append(out, trailerPrefix...)
	out = append(out, hex.EncodeToString(sum[:])...)
	return append(out, "\"}\n"...)
}

func readAll(b []byte, id string) (*Header, []Entry, error) {
	rd, err := NewReader(bytes.NewReader(b), int64(len(b)), id)
	if err != nil {
		return nil, nil, err
	}
	var out []Entry
	for {
		e, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return rd.Header(), out, nil
		}
		if err != nil {
			return rd.Header(), out, err
		}
		out = append(out, *e)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	h := testHeader(testID)
	h.CacheBase = "20261012T120000Z"
	h.Flood = []Flood{{Count: 9}, {At: []byte("d\xff"), HasAt: true, Count: 3}}
	h.Counts.UnreadableNew = 2
	h.Counts.Badname = 1
	h.Bytes = ByteCounts{Total: 10, Read: 11, New: 12}
	h.Blobs = BlobCounts{New: 1, Repaired: 1}
	entries := []Entry{
		mDirE("d"),
		mFileE("d/f", "x"),
		{Path: []byte("d/s"), Type: TypeFile, HasMeta: true, Size: 5, State: StateSkipped, Reason: ReasonRead},
		mLinkE("d/z", "../\u2028\x01\"\\"),
		{Path: []byte("d\xff"), Type: TypeDir, State: StateTruncated, Reason: ReasonEntries, Listing: ListingUnreadable},
		{Path: []byte("e"), Type: TypeUnknown, State: StateUnreadable},
		{Path: []byte("l"), Type: TypeLink, Target: []byte("t\xfe"), HasTarget: true},
	}
	b := encode(t, h, entries)
	got, es, err := readAll(b, testID)
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if !reflect.DeepEqual(es, entries) {
		t.Fatalf("entries differ:\n%+v\n%+v", es, entries)
	}
	if got.Complete || len(got.Partial) != 2 || !bytes.Equal(got.Partial[1].At, []byte("d\xff")) {
		t.Fatalf("partial = %+v", got.Partial)
	}
	if !got.Created.Equal(h.Created) || got.CacheBase != h.CacheBase || got.KnownBase != "" ||
		!reflect.DeepEqual(got.Flood, h.Flood) || !reflect.DeepEqual(got.Counts, h.Counts) || got.Bytes != h.Bytes ||
		got.Blobs != h.Blobs || got.Limits != h.Limits || got.Root != h.Root || !bytes.Equal(got.Tree, h.Tree) {
		t.Fatalf("header differs:\n%+v\n%+v", got, h)
	}
	if !strings.Contains(string(b), `\u2028`) || !strings.Contains(string(b), `\u0001`) {
		t.Fatalf("escapes missing: %s", b)
	}
}

func TestManifestInvalidUTF8NamesStayDistinct(t *testing.T) {
	h := testHeader(testID)
	entries := []Entry{mFileE("x\xfe", "a"), mFileE("x\xff", "b")}
	b := encode(t, h, entries)
	_, es, err := readAll(b, testID)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 || !bytes.Equal(es[0].Path, []byte("x\xfe")) || !bytes.Equal(es[1].Path, []byte("x\xff")) {
		t.Fatalf("names merged: %+v", es)
	}
	if bytes.Contains(b, []byte("\xef\xbf\xbd")) {
		t.Fatal("U+FFFD in the manifest")
	}
}

func TestManifestRootUnreadable(t *testing.T) {
	h := testHeader(testID)
	h.Root = RootInfo{State: StateUnreadable}
	b := encode(t, h, nil)
	got, _, err := readAll(b, testID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Complete || got.Root.State != StateUnreadable {
		t.Fatalf("root = %+v complete=%v", got.Root, got.Complete)
	}
	h1 := testHeader(testID)
	h1.Root.State = StateListingUnstable
	b = encode(t, h1, []Entry{mDirE("a"), mFileE("a/b", "x"), Entry{Path: []byte("m"), Type: TypeDir, State: StateMount}, mFileE("n", "y")})
	if got, _, err = readAll(b, testID); err != nil || !got.Complete {
		t.Fatalf("a racy root clears complete: %v %+v", err, got)
	}
	h2 := testHeader(testID)
	h2.Root = RootInfo{State: StateTruncated, Reason: ReasonTime}
	entries := []Entry{{Path: []byte("a"), Type: TypeDir, State: StateTruncated, Reason: ReasonTime}}
	b = encode(t, h2, entries)
	got, _, err = readAll(b, testID)
	if err != nil {
		t.Fatal(err)
	}
	want := []Partial{{State: StateTruncated, Reason: ReasonTime, Count: 2}}
	if !reflect.DeepEqual(got.Partial, want) {
		t.Fatalf("partial = %+v", got.Partial)
	}
}

// TestManifestRejectsCrafted runs crafted and corrupt manifests through the
// validator: every one must fail with ErrInvalid.
func TestManifestRejectsCrafted(t *testing.T) {
	base := []Entry{mDirE("a"), mFileE("a/b", "x"), mFileE("c", "y")}
	valid := func() []byte { return encode(t, testHeader(testID), slices.Clone(base)) }
	edit := func(old, new string) []byte {
		b := valid()
		if !bytes.Contains(b, []byte(old)) {
			t.Fatalf("%q not in manifest", old)
		}
		return retrailer(bytes.Replace(b, []byte(old), []byte(new), 1))
	}
	withEntries := func(es ...Entry) []byte { return encode(t, testHeader(testID), es) }
	headerEdit := func(f func(h *Header)) []byte {
		h := testHeader(testID)
		es := slices.Clone(base)
		Summarize(h, es)
		f(h)
		return encodeRaw(t, h, es)
	}
	long := strings.Repeat("x", MaxLineBytes)

	cases := []struct {
		name string
		b    []byte
		id   string
	}{
		{"link with a child", withEntries(mLinkE("a", "/etc"), mFileE("a/b", "x")), ""},
		{"file with a child", withEntries(mFileE("a", "x"), mFileE("a/b", "x")), ""},
		{"missing parent", withEntries(mFileE("a/b", "x")), ""},
		{"duplicate entries", withEntries(mFileE("a", "x"), mFileE("a", "x")), ""},
		{"out of order", withEntries(mFileE("b", "x"), mFileE("a", "x")), ""},
		{"dot-dot component", withEntries(mDirE("a"), mFileE("a/../b", "x")), ""},
		{"dot component", withEntries(mFileE(".", "x")), ""},
		{"empty component", withEntries(mDirE("a"), mFileE("a//b", "x")), ""},
		{"absolute path", withEntries(mFileE("/a", "x")), ""},
		{"NUL in path", withEntries(mFileE("a\x00b", "x")), ""},
		{"component too long", withEntries(mFileE(strings.Repeat("n", MaxComponentCap+1), "x")), ""},
		{"path over header limit", headerEditEntries(t, func(h *Header) { h.Limits.Path = 3 }, mFileE("abcd", "x")), ""},
		{"duplicate key", edit(`{"path":"c"`, `{"path":"c","path":"d"`), ""},
		{"path and path_b64", edit(`{"path":"c"`, `{"path":"c","path_b64":"/w=="`), ""},
		{"unknown entry field", edit(`{"path":"c"`, `{"path":"c","colour":"red"`), ""},
		{"unknown header field", edit(`{"version":1`, `{"version":1,"extra":0`), ""},
		{"duplicate header key", edit(`{"version":1`, `{"version":1,"version":1`), ""},
		{"non-canonical path_b64", edit(`{"path":"c"`, `{"path_b64":"Yw=="`), ""},
		{"base64 with bad padding bits", edit(`{"path":"c"`, `{"path_b64":"/x=="`), ""},
		{"base64 with escaped newline", edit(`{"path":"c"`, `{"path_b64":"/w\n=="`), ""},
		{"junk after the trailer", append(valid(), "junk\n"...), ""},
		{"second trailer", func() []byte { b := valid(); return append(b, b[bytes.LastIndex(b[:len(b)-1], []byte("\n"))+1:]...) }(), ""},
		{"bad trailer", bytes.Replace(valid(), []byte(`{"path":"c"`), []byte(`{"path":"d"`), 1), ""},
		{"no trailer", func() []byte { b := valid(); return b[:bytes.LastIndex(b[:len(b)-1], []byte("\n"))+1] }(), ""},
		{"no final newline", func() []byte { b := valid(); return b[:len(b)-1] }(), ""},
		{"empty", nil, ""},
		{"entry line too long", edit(`{"path":"c"`, `{"path":"c`+long+`"`), ""},
		{"header line too long", edit(`"lever_version":"0.38.0"`, `"lever_version":"`+strings.Repeat(long, 4)+`"`), ""},
		{"whitespace", edit(`{"path":"c"`, `{"path": "c"`), ""},
		{"raw control byte", edit(`{"path":"c"`, "{\"path\":\"c\x01\""), ""},
		{"raw invalid UTF-8", edit(`{"path":"c"`, "{\"path\":\"c\xff\""), ""},
		{"lone surrogate", edit(`{"path":"c"`, `{"path":"c\ud800"`), ""},
		{"leading zero", edit(`"nlink":1`, `"nlink":01`), ""},
		{"fraction", edit(`"nlink":1`, `"nlink":1.0`), ""},
		{"negative size", edit(`"size":1`, `"size":-1`), ""},
		{"mode over 12 bits", edit(`"mode":420`, `"mode":4096`), ""},
		{"partial metadata", edit(`,"nlink":1`, ``), ""},
		{"sha256 without verified", edit(`,"verified":100`, ``), ""},
		{"uppercase sha256", edit(`"sha256":"`+sumOfString("y")[:2], `"sha256":"`+strings.ToUpper(sumOfString("y")[:2])), ""},
		{"unknown type", edit(`"type":"file"`, `"type":"door"`), ""},
		{"unknown state", edit(`"verified":100}`, `"verified":100,"state":"fine"}`), ""},
		{"reason without state", withEntries(Entry{Path: []byte("a"), Type: TypeFile, Reason: ReasonRead}), ""},
		{"skipped without reason", withEntries(Entry{Path: []byte("a"), Type: TypeFile, State: StateSkipped}), ""},
		{"skipped with a truncated reason", withEntries(Entry{Path: []byte("a"), Type: TypeFile, State: StateSkipped, Reason: ReasonDepth}), ""},
		{"truncated file", withEntries(Entry{Path: []byte("a"), Type: TypeFile, State: StateTruncated, Reason: ReasonDepth}), ""},
		{"sha256 on a dir", withEntries(Entry{Path: []byte("a"), Type: TypeDir, SHA256: sumOfString("x"), Verified: 1}), ""},
		{"sha256 on a skipped file", withEntries(Entry{Path: []byte("a"), Type: TypeFile, SHA256: sumOfString("x"), Verified: 1, State: StateSkipped, Reason: ReasonRead}), ""},
		{"listing on a file", withEntries(Entry{Path: []byte("a"), Type: TypeFile, Listing: ListingUnreadable}), ""},
		{"unknown listing", withEntries(Entry{Path: []byte("a"), Type: TypeDir, Listing: "partly"}), ""},
		{"target on a file", withEntries(Entry{Path: []byte("a"), Type: TypeFile, Target: []byte("x"), HasTarget: true}), ""},
		{"link without target or state", withEntries(Entry{Path: []byte("a"), Type: TypeLink}), ""},
		{"target too long", withEntries(mLinkE("a", strings.Repeat("t", MaxTargetCap+1))), ""},
		{"file with no state and no content", withEntries(Entry{Path: []byte("a"), Type: TypeFile, HasMeta: true}), ""},
		{"file with content but no metadata", withEntries(Entry{Path: []byte("a"), Type: TypeFile, SHA256: sumOfString("x"), Verified: 1}), ""},
		{"secret without sha256", withEntries(Entry{Path: []byte("a"), Type: TypeFile, HasMeta: true, State: StateSecret}), ""},
		{"entry under a mount", withEntries(Entry{Path: []byte("m"), Type: TypeDir, State: StateMount}, mFileE("m/x", "x")), ""},
		{"entry under an excluded dir", withEntries(Entry{Path: []byte("m"), Type: TypeDir, State: StateExcluded}, mDirE("m/d")), ""},
		{"unknown type without unreadable", withEntries(Entry{Path: []byte("a"), Type: TypeUnknown}), ""},
		{"entry under an unreadable root", headerEditEntries(t, func(h *Header) { h.Root = RootInfo{State: StateUnreadable} }, mFileE("a", "x")), ""},
		{"id does not match the file", valid(), "20261013T120001Z"},
		{"bad file id", valid(), "latest"},
		{"seq 0", headerEdit(func(h *Header) { h.Seq = 0 }), ""},
		{"version 2", headerEdit(func(h *Header) { h.Version = 2 }), ""},
		{"unknown trigger", headerEdit(func(h *Header) { h.Trigger = "cron" }), ""},
		{"limits over the caps", headerEdit(func(h *Header) { h.Limits.Path = MaxPathCap + 1 }), ""},
		{"zero limit", headerEdit(func(h *Header) { h.Limits.Entries = 0 }), ""},
		{"wrong type count", headerEdit(func(h *Header) { h.Counts.ByType[TypeFile]++ }), ""},
		{"wrong state count", headerEdit(func(h *Header) { h.Counts.ByState[StateSecret] = 1 }), ""},
		{"complete when not", headerEdit(func(h *Header) { h.Root.Listing = ListingUnreadable }), ""},
		{"partial invented", headerEdit(func(h *Header) {
			h.Partial = []Partial{{State: StateSkipped, Reason: ReasonRead, At: []byte("c"), HasAt: true, Count: 1}}
		}), ""},
		{"too many flood elements", headerEdit(func(h *Header) { h.Flood = make([]Flood, 4) }), ""},
		{"missing counts key", edit(`,"badname":0`, ``), ""},
		{"missing by_type key", edit(`,"unknown":0},"by_state"`, `},"by_state"`), ""},
		{"non-UTC time", edit(`"created":"2026-10-13T12:00:00.123456789Z"`, `"created":"2026-10-13T14:00:00.123456789+02:00"`), ""},
		{"bad cache base", edit(`"cache_base":null`, `"cache_base":"../x"`), ""},
		{"tree and tree_b64", edit(`"tree":"/home/u/inst/tree"`, `"tree":"/a","tree_b64":"/w=="`), ""},
		{"no tree", edit(`"tree":"/home/u/inst/tree",`, ``), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.id
			if id == "" {
				id = testID
			}
			_, _, err := readAll(tc.b, id)
			t.Log(err)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid\n%s", err, tc.b)
			}
		})
	}
	if _, _, err := readAll(valid(), testID); err != nil {
		t.Fatalf("the unedited manifest is invalid: %v", err)
	}
}

func headerEditEntries(t *testing.T, f func(h *Header), es ...Entry) []byte {
	h := testHeader(testID)
	f(h)
	return encode(t, h, es)
}

// TestLongEscapedHeaderStillValidates: the header quotes the first path of
// every partial group and the flood directories. Agent-chosen names of
// control bytes grow sixfold when escaped, and the header must still pass
// the self-check, or the agent could prevent the snapshot.
func TestLongEscapedHeaderStillValidates(t *testing.T) {
	c1, c2, c3 := strings.Repeat("\x01", 1000), strings.Repeat("\x02", 1000), strings.Repeat("\x03", 1000)
	chain := c1 + "/" + c2 + "/" + c3
	entries := []Entry{mDirE(c1), mDirE(c1 + "/" + c2), mDirE(chain)}
	leaf := func(k int) string { return chain + "/" + strings.Repeat("\x04", 900) + string(rune('a'+k)) }
	k := 0
	for _, r := range TruncatedReasons {
		entries = append(entries, Entry{Path: []byte(leaf(k)), Type: TypeDir, State: StateTruncated, Reason: r})
		k++
	}
	for _, r := range SkippedReasons {
		entries = append(entries, Entry{Path: []byte(leaf(k)), Type: TypeFile, State: StateSkipped, Reason: r})
		k++
	}
	h := testHeader(testID)
	h.Tree = []byte(strings.Repeat("\x05", 4096))
	for i := 0; i < 3; i++ {
		h.Flood = append(h.Flood, Flood{At: []byte(leaf(i)), HasAt: true, Count: 1})
	}
	b := encode(t, h, entries)
	if n := bytes.IndexByte(b, '\n'); n <= MaxLineBytes {
		t.Fatalf("header is only %d bytes; the test needs one over MaxLineBytes", n)
	}
	for _, line := range bytes.Split(b[bytes.IndexByte(b, '\n')+1:], []byte("\n")) {
		if len(line)+1 > MaxLineBytes {
			t.Fatalf("an entry line is %d bytes", len(line)+1)
		}
	}
	got, _, err := readAll(b, testID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Partial) != len(TruncatedReasons)+len(SkippedReasons) {
		t.Fatalf("partial = %d groups", len(got.Partial))
	}
}

func TestManifestSizeCap(t *testing.T) {
	b := encode(t, testHeader(testID), []Entry{mFileE("a", "x")})
	if _, err := NewReader(bytes.NewReader(b), MaxManifestCap+1, testID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("over the hard cap: %v", err)
	}
	h := testHeader(testID)
	h.Limits.Manifest = 1
	b = encode(t, h, []Entry{mFileE("a", "x")})
	if _, err := NewReader(bytes.NewReader(b), 1+ManifestSlack+1, testID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("over limits.manifest + slack: %v", err)
	}
	if _, _, err := readAll(b, testID); err != nil {
		t.Fatalf("within the cap: %v", err)
	}
}

func TestEncodeEntryLengthIsTheLine(t *testing.T) {
	e := mLinkE("\x01\x01\x01", strings.Repeat("\x01", 10))
	line := EncodeEntry(nil, &e)
	if line[len(line)-1] != '\n' || bytes.Count(line, []byte("\n")) != 1 {
		t.Fatalf("not one line: %q", line)
	}
	if len(line) < 6*13 {
		t.Fatalf("escapes not counted: %d bytes", len(line))
	}
}

// TestManifestRandomRoundTrip writes random trees with names of random
// bytes (control bytes and invalid UTF-8 included) and reads back exactly
// what was written.
func TestManifestRandomRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for iter := 0; iter < 200; iter++ {
		entries := randomTree(r)
		h := testHeader(testID)
		b := encode(t, h, entries)
		got, es, err := readAll(b, testID)
		if err != nil {
			t.Fatalf("iteration %d: %v", iter, err)
		}
		if !reflect.DeepEqual(es, entries) {
			for i := range es {
				if !reflect.DeepEqual(es[i], entries[i]) {
					t.Fatalf("iteration %d entry %d:\n got %+v\nwant %+v", iter, i, es[i], entries[i])
				}
			}
			t.Fatalf("iteration %d: %d entries, want %d", iter, len(es), len(entries))
		}
		if got.Complete != h.Complete || !reflect.DeepEqual(got.Partial, h.Partial) {
			t.Fatalf("iteration %d: summary differs", iter)
		}
	}
}

func randomName(r *rand.Rand) []byte {
	for {
		n := 1 + r.IntN(12)
		b := make([]byte, n)
		for i := range b {
			switch r.IntN(4) {
			case 0:
				b[i] = byte(r.IntN(0x20))
			case 1:
				b[i] = byte(0x80 + r.IntN(0x80))
			default:
				b[i] = byte(0x20 + r.IntN(0x60))
			}
			if b[i] == '/' || b[i] == 0 {
				b[i] = 'q'
			}
		}
		if r.IntN(8) == 0 {
			b = append(b, "\u2028é"...)
		}
		if string(b) != "." && string(b) != ".." {
			return b
		}
	}
}

func randomTree(r *rand.Rand) []Entry {
	var out []Entry
	var walk func(dir []byte, depth int)
	walk = func(dir []byte, depth int) {
		n := r.IntN(6)
		seen := map[string]bool{}
		var kids []Entry
		for i := 0; i < n; i++ {
			name := randomName(r)
			if seen[string(name)] {
				continue
			}
			seen[string(name)] = true
			p := slices.Clone(name)
			if len(dir) > 0 {
				p = append(append(slices.Clone(dir), '/'), name...)
			}
			kids = append(kids, randomEntry(r, p, depth))
		}
		slices.SortFunc(kids, func(a, b Entry) int { return ComparePaths(a.Path, b.Path) })
		for _, k := range kids {
			out = append(out, k)
			if k.Type == TypeDir && depth < 3 {
				walk(k.Path, depth+1)
			}
		}
	}
	walk(nil, 0)
	return out
}

func randomEntry(r *rand.Rand, p []byte, depth int) Entry {
	e := Entry{Path: p}
	if r.IntN(5) > 0 {
		e.HasMeta = true
		e.Mode = uint32(r.IntN(0o10000))
		e.Size = r.Int64N(1 << 40)
		e.Mtime = r.Int64() - r.Int64()
		e.Ctime = r.Int64()
		e.Dev, e.Ino, e.Nlink = r.Uint64(), r.Uint64(), r.Uint64()
	}
	switch r.IntN(6) {
	case 0, 1:
		e.Type = TypeDir
		switch r.IntN(6) {
		case 0:
			e.State, e.Reason = StateTruncated, TruncatedReasons[r.IntN(len(TruncatedReasons))]
		case 1:
			e.Listing = ListingUnreadable
		case 2:
			e.State = StateListingUnstable
		}
	case 2, 3:
		e.Type = TypeFile
		switch r.IntN(5) {
		case 0:
			e.State, e.Reason = StateSkipped, SkippedReasons[r.IntN(len(SkippedReasons))]
		case 1:
			e.State = StateTooBig
		default:
			e.HasMeta = true
			e.SHA256 = sumOfString(string(p))
			e.Verified = r.Int64N(1 << 40)
			if r.IntN(4) == 0 {
				e.State = StateUnstable
			}
		}
	case 4:
		e.Type = TypeLink
		if r.IntN(5) == 0 {
			e.State = StateUnreadable
		} else {
			e.HasTarget = true
			e.Target = []byte{}
			for range r.IntN(20) {
				e.Target = append(e.Target, byte(1+r.IntN(255)))
			}
		}
	default:
		e.Type = []EntryType{TypeFIFO, TypeSocket, TypeChar, TypeBlock}[r.IntN(4)]
		if r.IntN(3) == 0 {
			e.State = StateExcluded
		}
	}
	return e
}
