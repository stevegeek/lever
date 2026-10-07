package chatfiles

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/scion"
)

var t0 = time.Date(2026, 10, 7, 10, 15, 0, 0, time.UTC)

func TestContainerWorkspaceIsScions(t *testing.T) {
	if ContainerWorkspace != scion.ContainerWorkspace {
		t.Fatalf("%q != %q", ContainerWorkspace, scion.ContainerWorkspace)
	}
}

func TestKey(t *testing.T) {
	k := Key("C@Example.com")
	if k != Key("c@example.com") || len(k) != 25 || k[0] != 'k' || k == Key("d@example.com") {
		t.Fatalf("key %q", k)
	}
	for _, c := range k[1:] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("key %q", k)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	long := strings.Repeat("a", 300) + ".pdf"
	for in, want := range map[string]string{
		"Report (final).pdf":     "Report _final_.pdf",
		"../../etc/passwd.pdf":   "passwd.pdf",
		`a\b.pdf`:                "b.pdf",
		".bashrc.pdf":            "_bashrc.pdf",
		"  ..x.pdf. ":            "_.x.pdf",
		"-rf.pdf":                "_rf.pdf",
		" -x.pdf":                "_x.pdf",
		"CON.pdf":                "_CON.pdf",
		"nul":                    "_nul",
		"com1 .tar.zip":          "_com1 .tar.zip",
		"Lpt9.xlsx":              "_Lpt9.xlsx",
		"COM10.pdf":              "COM10.pdf",
		"console.pdf":            "console.pdf",
		".":                      "file",
		"-":                      "_",
		"x\u202e.pdf":            "x___.pdf",
		"Fattura è.pdf":          "Fattura __.pdf",
		"":                       "file",
		"...":                    "file",
		"a\x00b.pdf":             "a_b.pdf",
		long:                     strings.Repeat("a", 116) + ".pdf",
		strings.Repeat("b", 130): strings.Repeat("b", 120),
	} {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
		if got := SanitizeName(in); len(got) > MaxNameLen || SanitizeName(got) != got {
			t.Errorf("SanitizeName(%q) = %q is not stable or too long", in, got)
		}
	}
}

func TestExtAllowed(t *testing.T) {
	for name, want := range map[string]bool{"a.pdf": true, "a.PDF": true, "a.xlsm": true, "a.exe": false, "a": false, "a.pdf.exe": false, ".pdf": false} {
		if got := ExtAllowed(name, DefaultExtensions); got != want {
			t.Errorf("%q: %v, want %v", name, got, want)
		}
	}
}

func TestStoreWritesHashesAndRetriesATakenName(t *testing.T) {
	tree := t.TempDir()
	dir := InDir("workers/w1", "c@x")
	s1, err := Store(tree, dir, "a.pdf", strings.NewReader("%PDF-1"), 100, t0)
	if err != nil {
		t.Fatal(err)
	}
	if s1.Rel != dir+"/20261007T101500Z-a.pdf" || s1.Size != 6 || len(s1.SHA256) != 64 {
		t.Fatalf("%+v", s1)
	}
	fi, _ := os.Lstat(filepath.Join(tree, s1.Rel))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	for _, d := range []string{"workers/w1/" + Dir, "workers/w1/" + Dir + "/in", dir} {
		if fi, err := os.Lstat(filepath.Join(tree, d)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, fi, err)
		}
	}
	s2, err := Store(tree, dir, "a.pdf", strings.NewReader("x"), 100, t0)
	if err != nil || s2.Rel != dir+"/20261007T101500Z-2-a.pdf" {
		t.Fatalf("second = %+v %v", s2, err)
	}
	sha, size, err := Hash(context.Background(), tree, s1.Rel, 100)
	if err != nil || sha != s1.SHA256 || size != 6 {
		t.Fatalf("hash %s %d %v", sha, size, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Hash(ctx, tree, s1.Rel, 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("a done context: %v", err)
	}
}

func TestStoreTooLargeLeavesNothing(t *testing.T) {
	tree := t.TempDir()
	dir := InDir("w", "c@x")
	if _, err := Store(tree, dir, "a.pdf", strings.NewReader("12345"), 4, t0); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	ents, _ := os.ReadDir(filepath.Join(tree, dir))
	if len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
}

func TestStoreGivesUpAfterTenTakenNames(t *testing.T) {
	tree := t.TempDir()
	dir := InDir("w", "c@x")
	for n := 1; n <= 10; n++ {
		if _, err := Store(tree, dir, "a.pdf", strings.NewReader("x"), 10, t0); err != nil {
			t.Fatal(n, err)
		}
	}
	if _, err := Store(tree, dir, "a.pdf", strings.NewReader("x"), 10, t0); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v", err)
	}
}

func TestStoreRefusesALinkOnItsDir(t *testing.T) {
	tree, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "w"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, "w", Dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := Store(tree, InDir("w", "c@x"), "a.pdf", strings.NewReader("x"), 10, t0); !errors.Is(err, fsutil.ErrSymlink) {
		t.Fatalf("err = %v", err)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("wrote through the link: %v", ents)
	}
}

func TestCopyVerifiedRefusesChangedBytes(t *testing.T) {
	tree := t.TempDir()
	s, err := Store(tree, OutDir("w", "c@x"), "v3.xlsm", strings.NewReader("ORIGINAL"), 100, t0)
	if err != nil {
		t.Fatal(err)
	}
	f, n, err := CopyVerified(tree, s.Rel, s.SHA256, 100)
	if err != nil || n != 8 {
		t.Fatal(n, err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "ORIGINAL" {
		t.Fatalf("copy %q", b)
	}
	// The same size, rewritten in place: the inode and the length stay.
	if err := os.WriteFile(filepath.Join(tree, s.Rel), []byte("SWAPPED!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CopyVerified(tree, s.Rel, s.SHA256, 100); !errors.Is(err, ErrChanged) {
		t.Fatalf("rewritten: %v", err)
	}
	_ = os.Remove(filepath.Join(tree, s.Rel))
	if _, _, err := CopyVerified(tree, s.Rel, s.SHA256, 100); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted: %v", err)
	}
}

func TestCopyVerifiedServesItsOwnCopy(t *testing.T) {
	tree := t.TempDir()
	s, _ := Store(tree, OutDir("w", "c@x"), "v.txt", strings.NewReader("ORIGINAL"), 100, t0)
	f, _, err := CopyVerified(tree, s.Rel, s.SHA256, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// A rewrite after the check does not reach what is served.
	_ = os.WriteFile(filepath.Join(tree, s.Rel), []byte("SWAPPED!"), 0o644)
	b, _ := io.ReadAll(f)
	if !bytes.Equal(b, []byte("ORIGINAL")) {
		t.Fatalf("served %q", b)
	}
	if _, err := os.Stat(f.Name()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the copy is still named on disk: %v", err)
	}
}

func TestShareRel(t *testing.T) {
	k := Key("c@x")
	ok := Dir + "/out/" + k + "/v3.xlsm"
	for p, want := range map[string]string{
		ok:                                    "workers/w1/" + ok,
		"/workspace/" + ok:                    "workers/w1/" + ok,
		Dir + "/in/" + k + "/v3.xlsm":         "",
		Dir + "/out/" + Key("d@x") + "/a.pdf": "",
		Dir + "/out/" + k + "/sub/a.pdf":      "",
		Dir + "/out/" + k + "/../in/a.pdf":    "",
		Dir + "/out/" + k + "/.hidden.pdf":    "",
		Dir + "/out/" + k + "/a b(1).pdf":     "",
		Dir + "/out/" + k + "/":               "",
		"/etc/passwd":                         "",
		"/workspace/../lever/" + ok:           "",
		Dir + "/out/" + k + `/a\b.pdf`:        "",
	} {
		rel, _, err := ShareRel("workers/w1", "c@x", p)
		if want == "" && !errors.Is(err, ErrBadPath) || want != "" && (err != nil || rel != want) {
			t.Errorf("%q: %q %v, want %q", p, rel, err, want)
		}
	}
	if rel, name, err := ShareRel(".", "c@x", ok); err != nil || rel != ok || name != "v3.xlsm" {
		t.Errorf("manager: %q %q %v", rel, name, err)
	}
}

func TestContainerPath(t *testing.T) {
	if got := ContainerPath("workers/w1", "workers/w1/.lever-files/in/k/a.pdf"); got != "/workspace/.lever-files/in/k/a.pdf" {
		t.Fatal(got)
	}
	if got := ContainerPath(".", ".lever-files/in/k/a.pdf"); got != "/workspace/.lever-files/in/k/a.pdf" {
		t.Fatal(got)
	}
}

func TestStageIsPrivateAndBounded(t *testing.T) {
	f, sha, n, err := Stage(strings.NewReader("hello"), 10)
	if err != nil || n != 5 || len(sha) != 64 {
		t.Fatal(sha, n, err)
	}
	defer f.Close()
	if _, err := os.Stat(f.Name()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staged file is still named on disk: %v", err)
	}
	if b, _ := io.ReadAll(f); string(b) != "hello" {
		t.Fatalf("%q", b)
	}
	if _, _, _, err := Stage(strings.NewReader("12345"), 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("client went away") }

func TestStageTellsABodyErrorFromAHostFault(t *testing.T) {
	if _, _, _, err := Stage(failingReader{}, 10); err == nil || errors.Is(err, ErrStage) {
		t.Fatalf("a body error is not a host fault: %v", err)
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	if _, _, _, err := Stage(strings.NewReader("x"), 10); !errors.Is(err, ErrStage) {
		t.Fatalf("no temp dir: %v", err)
	}
}
