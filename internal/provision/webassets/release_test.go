package webassets

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/scion/layout"
)

type entry struct {
	hdr  tar.Header
	body string
}

func file(name, body string) entry {
	return entry{tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(body))}, body}
}

func dir(name string) entry {
	return entry{hdr: tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: 0o755}}
}

func archive(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := e.hdr
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
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

var sentinel = file(layout.WebAssetsSentinel, "console.log(1)")

func TestUnpackReleaseWritesAPlainTree(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "client")
	a := archive(t, dir("assets/"), sentinel,
		entry{tar.Header{Typeflag: tar.TypeReg, Name: "index.html", Mode: 0o4777, Size: 5}, "hello"})
	if err := unpackRelease(bytes.NewReader(a), dst); err != nil {
		t.Fatalf("unpackRelease: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dst, "index.html"))
	if err != nil || fi.Mode() != 0o644 {
		t.Fatalf("index.html: %v, mode %v; want 0644 whatever the archive says", err, fi.Mode())
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "index.html")); string(b) != "hello" {
		t.Fatalf("index.html = %q", b)
	}
	if fi, err := os.Stat(filepath.Join(dst, "assets")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("assets dir: %v", err)
	}
}

func TestUnpackReleaseRefuses(t *testing.T) {
	big := strings.Repeat("x", 64)
	for name, c := range map[string]struct {
		entries []entry
		limit   func()
	}{
		"parent traversal":    {entries: []entry{sentinel, file("../escape", "x")}},
		"inner traversal":     {entries: []entry{sentinel, file("assets/../../escape", "x")}},
		"absolute path":       {entries: []entry{sentinel, file("/tmp/escape", "x")}},
		"dot path":            {entries: []entry{sentinel, file("./index.html", "x")}},
		"empty element":       {entries: []entry{sentinel, file("assets//x.js", "x")}},
		"backslash":           {entries: []entry{sentinel, file(`assets\..\..\x`, "x")}},
		"dot directory":       {entries: []entry{dir("./"), sentinel}},
		"traversal directory": {entries: []entry{dir("../up/"), sentinel}},
		"symlink": {entries: []entry{sentinel,
			{hdr: tar.Header{Typeflag: tar.TypeSymlink, Name: "index.html", Linkname: "/etc/passwd"}}}},
		"symlink then file through it": {entries: []entry{
			{hdr: tar.Header{Typeflag: tar.TypeSymlink, Name: "assets", Linkname: "/tmp"}}, sentinel}},
		"hardlink": {entries: []entry{sentinel,
			{hdr: tar.Header{Typeflag: tar.TypeLink, Name: "index.html", Linkname: layout.WebAssetsSentinel}}}},
		"char device": {entries: []entry{sentinel,
			{hdr: tar.Header{Typeflag: tar.TypeChar, Name: "null", Devmajor: 1, Devminor: 3}}}},
		"block device":              {entries: []entry{sentinel, {hdr: tar.Header{Typeflag: tar.TypeBlock, Name: "sda"}}}},
		"fifo":                      {entries: []entry{sentinel, {hdr: tar.Header{Typeflag: tar.TypeFifo, Name: "pipe"}}}},
		"duplicate file overwrites": {entries: []entry{sentinel, file(layout.WebAssetsSentinel, "evil")}},
		"file over a directory":     {entries: []entry{dir("assets/"), sentinel, file("assets", "x")}},
		"directory over a file":     {entries: []entry{file("x", "x"), dir("x/"), sentinel}},
		"file under a file":         {entries: []entry{file("x", "x"), file("x/y", "y"), sentinel}},
		"no sentinel":               {entries: []entry{file("index.html", "x")}},
		"empty":                     {},
		"oversize file": {
			entries: []entry{sentinel, file("big.js", big)},
			limit:   func() { maxReleaseFileBytes = 32 },
		},
		"oversize tree": {
			entries: []entry{sentinel, file("a.js", big), file("b.js", big)},
			limit:   func() { maxReleaseTreeBytes = 100 },
		},
		"too many files": {
			entries: []entry{sentinel, file("a.js", "a"), file("b.js", "b"), file("c.js", "c")},
			limit:   func() { maxReleaseEntries = 3 },
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, f, tr := maxReleaseEntries, maxReleaseFileBytes, maxReleaseTreeBytes
			t.Cleanup(func() { maxReleaseEntries, maxReleaseFileBytes, maxReleaseTreeBytes = e, f, tr })
			if c.limit != nil {
				c.limit()
			}
			root := t.TempDir()
			dst := filepath.Join(root, "out", "client")
			err := unpackRelease(bytes.NewReader(archive(t, c.entries...)), dst)
			if !errors.Is(err, ErrBadReleaseArchive) {
				t.Fatalf("err = %v, want ErrBadReleaseArchive", err)
			}
			// Nothing was written outside dst.
			var outside []string
			_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err == nil && p != root && p != filepath.Join(root, "out") && !strings.HasPrefix(p, dst) {
					outside = append(outside, p)
				}
				return nil
			})
			if len(outside) != 0 {
				t.Fatalf("wrote outside the destination: %v", outside)
			}
		})
	}
}

func TestUnpackReleaseRefusesNotGzip(t *testing.T) {
	err := unpackRelease(strings.NewReader("not a gzip"), filepath.Join(t.TempDir(), "client"))
	if !errors.Is(err, ErrBadReleaseArchive) {
		t.Fatalf("err = %v", err)
	}
}

func TestUnpackReleaseRefusesAnExistingDestination(t *testing.T) {
	dst := t.TempDir()
	if err := unpackRelease(bytes.NewReader(archive(t, sentinel)), dst); err == nil {
		t.Fatal("unpacked into a directory that already exists")
	}
}

// The release asset is reproducible: the same tree gives the same bytes
// whatever the files' times and modes on the build host.
func TestWriteReleaseArchiveIsDeterministic(t *testing.T) {
	mk := func(mtime time.Time, mode os.FileMode) string {
		d := t.TempDir()
		write(t, filepath.Join(d, "assets", "main.js"), "console.log(1)")
		write(t, filepath.Join(d, "assets", "main.js.map"), "{}")
		write(t, filepath.Join(d, "index.html"), "<html>")
		for _, p := range []string{"assets/main.js", "index.html", "assets"} {
			if err := os.Chtimes(filepath.Join(d, p), mtime, mtime); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(filepath.Join(d, "index.html"), mode); err != nil {
			t.Fatal(err)
		}
		return d
	}
	out := t.TempDir()
	sumA, sizeA, err := writeReleaseArchive(filepath.Join(out, "a.tar.gz"), mk(time.Unix(1e9, 0), 0o600))
	if err != nil {
		t.Fatal(err)
	}
	sumB, sizeB, err := writeReleaseArchive(filepath.Join(out, "b.tar.gz"), mk(time.Unix(2e9, 0), 0o755))
	if err != nil {
		t.Fatal(err)
	}
	if sumA != sumB || sizeA != sizeB {
		t.Fatalf("archives differ: %s/%d vs %s/%d", sumA, sizeA, sumB, sizeB)
	}
	if fi, _ := os.Stat(filepath.Join(out, "a.tar.gz")); fi.Size() != sizeA {
		t.Fatalf("size %d, file is %d", sizeA, fi.Size())
	}
	f, err := os.Open(filepath.Join(out, "a.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || !h.ModTime.Equal(time.Unix(0, 0)) {
			t.Fatalf("%s carries host metadata: %+v", h.Name, h)
		}
		names = append(names, h.Name)
	}
	if want := []string{"assets/", "assets/main.js", "index.html"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v (sorted, no sourcemaps)", names, want)
	}
}

func TestWriteReleaseArchiveRefusesALink(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "assets", "main.js"), "x")
	if err := os.Symlink("main.js", filepath.Join(d, "assets", "link.js")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeReleaseArchive(filepath.Join(t.TempDir(), "a.tar.gz"), d); err == nil {
		t.Fatal("archived a symlink the unpack would refuse")
	}
}

func TestImportRelease(t *testing.T) {
	src, assets := t.TempDir(), t.TempDir()
	good := archive(t, dir("assets/"), sentinel)
	write(t, filepath.Join(src, "scion-web_abc1234.tar.gz"), string(good))
	write(t, filepath.Join(assets, "scion-web_old.tar.gz"), "stale")
	a, err := ImportRelease(src, assets, "abc1234")
	if err != nil {
		t.Fatalf("ImportRelease: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(assets, a.Name))
	if err != nil || !bytes.Equal(b, good) || a.Size != int64(len(good)) || a.Name != "scion-web_abc1234.tar.gz" {
		t.Fatalf("asset %+v, err %v", a, err)
	}
	if h := sha256Hex(good); a.SHA256 != h {
		t.Fatalf("sha256 %s, want %s", a.SHA256, h)
	}
	if left, _ := filepath.Glob(filepath.Join(assets, "scion-web_*")); len(left) != 1 {
		t.Fatalf("assets = %v, want only the imported one", left)
	}

	// The release job refuses an archive lever would refuse to unpack.
	write(t, filepath.Join(src, "scion-web_abc1234.tar.gz"), string(archive(t, sentinel,
		entry{hdr: tar.Header{Typeflag: tar.TypeSymlink, Name: "index.html", Linkname: "/etc/passwd"}})))
	if _, err := ImportRelease(src, assets, "abc1234"); !errors.Is(err, ErrBadReleaseArchive) {
		t.Fatalf("err = %v, want ErrBadReleaseArchive", err)
	}
	if _, err := ImportRelease(src, assets, "other12"); err == nil {
		t.Fatal("imported an archive for another pin")
	}
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
