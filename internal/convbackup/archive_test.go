package convbackup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type entry struct {
	name string
	typ  byte
	body string
	link string
	size int64 // header size override for a regular file (0 = len(body))
	mt   time.Time
}

func mkTar(t *testing.T, es ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, ModTime: e.mt, Linkname: e.link}
		if e.mt.IsZero() {
			h.ModTime = time.Unix(1_700_000_000, 0)
		}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.body))
			if e.size != 0 {
				h.Size = e.size
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg && e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type readEntry struct {
	typ   byte
	mode  int64
	body  string
	mtime time.Time
	uid   int
	uname string
}

func readTar(t *testing.T, b []byte) map[string]readEntry {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	out := map[string]readEntry{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		out[h.Name] = readEntry{h.Typeflag, h.Mode, string(body), h.ModTime, h.Uid, h.Uname}
	}
}

// TestCopyKeepsFilesAndDirsOnly: the prefix goes, links and special files
// are skipped and counted, modes are 0700/0600 with no owner, the times stay
// and the newest transcript is named.
func TestCopyKeepsFilesAndDirsOnly(t *testing.T) {
	old, newer := time.Unix(1_700_000_000, 0).UTC(), time.Unix(1_800_000_000, 0).UTC()
	src := mkTar(t,
		entry{name: "projects/", typ: tar.TypeDir},
		entry{name: "projects/-workspace/", typ: tar.TypeDir},
		entry{name: "projects/-workspace/a.jsonl", typ: tar.TypeReg, body: "old\n", mt: old},
		entry{name: "projects/-workspace/b.jsonl", typ: tar.TypeReg, body: "new\n", mt: newer},
		entry{name: "projects/-workspace/evil", typ: tar.TypeSymlink, link: "/home/user/.ssh/id_ed25519"},
		entry{name: "projects/-workspace/hard", typ: tar.TypeLink, link: "projects/-workspace/a.jsonl"},
		entry{name: "projects/-workspace/fifo", typ: tar.TypeFifo},
	)
	var out bytes.Buffer
	st, err := Copy(&out, bytes.NewReader(src), "projects", DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 2 || st.Dirs != 1 || st.Skipped != 3 || st.Bytes != 8 {
		t.Fatalf("stats = %+v", st)
	}
	if st.Newest != "-workspace/b.jsonl" || !st.NewestTime.Equal(newer) {
		t.Fatalf("newest = %q %v", st.Newest, st.NewestTime)
	}
	got := readTar(t, out.Bytes())
	if len(got) != 3 {
		t.Fatalf("entries = %v", got)
	}
	d := got["-workspace/"]
	if d.typ != tar.TypeDir || d.mode != 0o700 {
		t.Fatalf("dir = %+v", d)
	}
	f := got["-workspace/a.jsonl"]
	if f.typ != tar.TypeReg || f.mode != 0o600 || f.body != "old\n" || !f.mtime.Equal(old) || f.uid != 0 || f.uname != "" {
		t.Fatalf("file = %+v", f)
	}
	// A stored archive copies through unchanged with no prefix (the restore
	// re-checks it this way).
	var again bytes.Buffer
	st2, err := Copy(&again, bytes.NewReader(out.Bytes()), "", DefaultLimits)
	if err != nil || st2.Files != 2 || st2.Newest != st.Newest {
		t.Fatalf("re-copy: %+v %v", st2, err)
	}
}

// TestCopyRefuses: every unsafe name, a conflict, and every limit refuses
// the whole archive.
func TestCopyRefuses(t *testing.T) {
	reg := func(name, body string) entry { return entry{name: name, typ: tar.TypeReg, body: body} }
	small := Limits{MaxFile: 10, MaxTotal: 15, MaxEntries: 4}
	cases := []struct {
		name string
		es   []entry
		lim  Limits
	}{
		{"dotdot", []entry{reg("projects/../../.bashrc", "x")}, DefaultLimits},
		{"dotdot inside", []entry{reg("projects/a/../../b", "x")}, DefaultLimits},
		{"absolute", []entry{reg("/etc/passwd", "x")}, DefaultLimits},
		{"outside prefix", []entry{reg("other/a.jsonl", "x")}, DefaultLimits},
		{"prefix lookalike", []entry{reg("projectsX/a.jsonl", "x")}, DefaultLimits},
		{"control char", []entry{reg("projects/a\x1b[2J.jsonl", "x")}, DefaultLimits},
		{"backslash", []entry{reg("projects/a\\b", "x")}, DefaultLimits},
		{"duplicate", []entry{reg("projects/a", "x"), reg("projects/a", "y")}, DefaultLimits},
		{"beneath a file", []entry{reg("projects/a", "x"), reg("projects/a/b", "y")}, DefaultLimits},
		{"file over an implied dir", []entry{reg("projects/a/b", "x"), reg("projects/a", "y")}, DefaultLimits},
		{"skipped data counts", []entry{reg("projects/a", "12345678"), {name: "projects/s", typ: tar.TypeGNUSparse, size: 1 << 40}}, small},
		{"file too large", []entry{reg("projects/a", strings.Repeat("x", 11))}, small},
		{"total too large", []entry{reg("projects/a", strings.Repeat("x", 8)), reg("projects/b", strings.Repeat("x", 8))}, small},
		{"too many entries", []entry{reg("projects/a", ""), reg("projects/b", ""), reg("projects/c", ""), reg("projects/d", ""), reg("projects/e", "")}, small},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Copy(io.Discard, bytes.NewReader(mkTar(t, c.es...)), "projects", c.lim)
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("err = %v, want ErrRefused", err)
			}
		})
	}
	if _, err := Copy(io.Discard, strings.NewReader("not gzip"), "projects", DefaultLimits); !errors.Is(err, ErrRefused) {
		t.Fatalf("not gzip: %v", err)
	}
}

// TestCopyRefusesATruncatedFile: a header that promises more bytes than the
// stream holds is refused, not padded.
func TestCopyRefusesATruncatedFile(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: "projects/a", Typeflag: tar.TypeReg, Size: 100, Mode: 0o600})
	_, _ = tw.Write([]byte("short"))
	zw.Write(nil)
	_ = zw.Close()
	if _, err := Copy(io.Discard, bytes.NewReader(buf.Bytes()), "projects", DefaultLimits); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v", err)
	}
}
