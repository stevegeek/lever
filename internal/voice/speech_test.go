package voice

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// testSpeechFiles is the content of a tiny Kokoro model's files, as
// testdata/tiny-kokoro.tar.bz2 holds them below its top directory
// tiny-kokoro/ (with a ._model.onnx that is skipped, an empty directory
// and a pax global header). The archive was made with Python's tarfile
// and bz2 modules, as no bzip2 writer is in Go's library.
var testSpeechFiles = map[string]string{
	"model.onnx":                          "onnx bytes",
	"voices.bin":                          "voice bytes",
	"tokens.txt":                          "a 1\nb 2\n",
	"espeak-ng-data/phontab":              "phontab",
	"espeak-ng-data/lang/gmw/en":          "name english",
	"espeak-ng-data/en_dict":              "dict",
	"espeak-ng-data/intonations":          "into",
	"espeak-ng-data/phondata":             "phondata",
	"espeak-ng-data/phonindex":            "phonindex",
	"espeak-ng-data/voices/!v/Mr serious": "variant",
}

const testSpeechArchive = "testdata/tiny-kokoro.tar.bz2"

// testSpeechModel is a Kokoro model pinned to testSpeechArchive.
func testSpeechModel() SpeechModel {
	b, err := os.ReadFile(testSpeechArchive)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return SpeechModel{
		Name: "tiny-kokoro", Kind: KindKokoro,
		ArchiveURL:  "https://github.com/someone/tiny/releases/download/models/tiny-kokoro.tar.bz2",
		ArchiveSize: int64(len(b)), ArchiveSHA256: hex.EncodeToString(sum[:]), ArchiveRoot: "tiny-kokoro",
		ModelFile: "model.onnx", TokensFile: "tokens.txt", VoicesFile: "voices.bin", DataDir: "espeak-ng-data",
		Voices:       []SpeechVoice{{"af", 0, "en-US"}, {"bf_emma", 7, "en-GB"}},
		DefaultVoice: "bf_emma", SampleRate: 24000,
	}
}

// testSpeechManifest is the manifest of testSpeechFiles.
func testSpeechManifest() []SpeechFile {
	var out []SpeechFile
	for p, c := range testSpeechFiles {
		sum := sha256.Sum256([]byte(c))
		out = append(out, SpeechFile{Path: p, Size: int64(len(c)), SHA256: hex.EncodeToString(sum[:])})
	}
	slices.SortFunc(out, func(a, b SpeechFile) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// placeSpeech writes m's files and manifest into dir as a fetch would.
func placeSpeech(t *testing.T, dir string, m SpeechModel) {
	t.Helper()
	root := SpeechDir(dir, m)
	for p, c := range testSpeechFiles {
		sub, err := privateDirs(root, path.Dir(p), true)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, path.Base(p)), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeSpeechManifest(root, m, testSpeechManifest()); err != nil {
		t.Fatal(err)
	}
}

func TestShippedSpeechModels(t *testing.T) {
	for _, name := range []string{"kokoro-en-v0_19", "piper-en_GB-alba-medium"} {
		m, ok := LookupSpeech(name)
		if !ok {
			t.Fatalf("%s is not in the table", name)
		}
		if why := m.Pinned(); why != "" {
			t.Errorf("%s: %s", name, why)
		}
		if !strings.HasPrefix(m.ArchiveURL, speechReleaseURL) || path.Base(m.ArchiveURL) != m.ArchiveRoot+".tar.bz2" {
			t.Errorf("%s: archive %s, top directory %s", name, m.ArchiveURL, m.ArchiveRoot)
		}
	}
	// sherpa-onnx's table for kokoro-en-v0_19: speaker id -> voice.
	k, _ := LookupSpeech(DefaultSpeechModel)
	for i, v := range []string{"af", "af_bella", "af_nicole", "af_sarah", "af_sky", "am_adam", "am_michael", "bf_emma", "bf_isabella", "bm_george", "bm_lewis"} {
		if got, ok := k.Voice(v); !ok || got.SID != i {
			t.Errorf("%s: %+v %v, want id %d", v, got, ok, i)
		}
	}
	if len(k.Voices) != 11 || k.DefaultVoice != "bf_emma" || k.SampleRate != 24000 {
		t.Errorf("%+v", k)
	}
}

func TestSpeechModelPinned(t *testing.T) {
	if why := testSpeechModel().Pinned(); why != "" {
		t.Fatal(why)
	}
	for name, mut := range map[string]func(m *SpeechModel){
		"unverified sha256": func(m *SpeechModel) { m.ArchiveSHA256 = Unverified },
		"short sha256":      func(m *SpeechModel) { m.ArchiveSHA256 = "abc" },
		"upper sha256":      func(m *SpeechModel) { m.ArchiveSHA256 = strings.ToUpper(m.ArchiveSHA256) },
		"zero size":         func(m *SpeechModel) { m.ArchiveSize = 0 },
		"http":              func(m *SpeechModel) { m.ArchiveURL = "http" + strings.TrimPrefix(m.ArchiveURL, "https") },
		"other host":        func(m *SpeechModel) { m.ArchiveURL = "https://example.com/x.tar.bz2" },
		"query":             func(m *SpeechModel) { m.ArchiveURL += "?x=1.tar.bz2" },
		"not bz2":           func(m *SpeechModel) { m.ArchiveURL = strings.TrimSuffix(m.ArchiveURL, ".bz2") + ".gz" },
		"root with slash":   func(m *SpeechModel) { m.ArchiveRoot = "a/b" },
		"root dotdot":       func(m *SpeechModel) { m.ArchiveRoot = ".." },
		"dotdot model":      func(m *SpeechModel) { m.ModelFile = "../x" },
		"absolute tokens":   func(m *SpeechModel) { m.TokensFile = "/x" },
		"no voices file":    func(m *SpeechModel) { m.VoicesFile = "" },
		"no data dir":       func(m *SpeechModel) { m.DataDir = "" },
		"bad default":       func(m *SpeechModel) { m.DefaultVoice = "zz" },
		"bad kind":          func(m *SpeechModel) { m.Kind = "tacotron" },
		"bad rate":          func(m *SpeechModel) { m.SampleRate = 0 },
		"no voices":         func(m *SpeechModel) { m.Voices = nil },
		"name with slash":   func(m *SpeechModel) { m.Name = "../x" },
	} {
		t.Run(name, func(t *testing.T) {
			m := testSpeechModel()
			mut(&m)
			if m.Pinned() == "" {
				t.Fatal("accepted")
			}
		})
	}
	// A VITS model needs no voices file.
	m := testSpeechModel()
	m.Kind, m.VoicesFile = KindVITS, ""
	if why := m.Pinned(); why != "" {
		t.Fatal(why)
	}
}

func TestSpeechInput(t *testing.T) {
	for in, want := range map[string]string{
		"Hello, world.":            "Hello, world.",
		"  two\n\nlines\t here ":   "two lines here",
		"--flag=1 then words":      "flag=1 then words",
		"— dash first":             "dash first",
		"a <speak>tag</speak> b":   "a speak tag /speak b",
		"bell\x07 and​ zero width": "bell and zero width",
		"bad \xff utf8":            "bad utf8",
	} {
		got, err := SpeechInput(in, 100)
		if err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "   ", "---", "!!! ...", strings.Repeat("a", 101)} {
		if _, err := SpeechInput(in, 100); !errors.Is(err, ErrBadText) {
			t.Errorf("%q accepted: %v", in, err)
		}
	}
	if _, err := SpeechInput(strings.Repeat("é", 100), 100); err != nil {
		t.Errorf("100 characters of two bytes: %v", err)
	}
}

func TestSherpaArgs(t *testing.T) {
	m := testSpeechModel()
	got := SherpaArgs(m, "/m", SpeechVoice{"bf_emma", 7, "en-GB"}, 1.25, 2, "/m/.speak-1/speech.wav", "Hello.")
	want := []string{
		"--kokoro-model=/m/tiny-kokoro/model.onnx", "--kokoro-voices=/m/tiny-kokoro/voices.bin",
		"--kokoro-tokens=/m/tiny-kokoro/tokens.txt", "--kokoro-data-dir=/m/tiny-kokoro/espeak-ng-data",
		"--kokoro-length-scale=0.800", "--num-threads=2", "--sid=7", "--output-filename=/m/.speak-1/speech.wav", "--", "Hello.",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
	m.Kind, m.ModelFile, m.VoicesFile = KindVITS, "alba.onnx", ""
	got = SherpaArgs(m, "/m", SpeechVoice{"alba", 0, "en-GB"}, 1, 1, "/o.wav", "Hi.")
	want = []string{"--vits-model=/m/tiny-kokoro/alba.onnx", "--vits-tokens=/m/tiny-kokoro/tokens.txt",
		"--vits-data-dir=/m/tiny-kokoro/espeak-ng-data", "--vits-length-scale=1.000", "--num-threads=1", "--sid=0", "--output-filename=/o.wav", "--", "Hi."}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
}

// engineWAV is a WAV as an engine might write it: a fmt chunk, extra
// chunks, and data.
func engineWAV(format, channels uint16, rate uint32, bits uint16, extra string, samples []byte, dataSize int) []byte {
	le := binary.LittleEndian
	b := []byte("RIFF\x00\x00\x00\x00WAVEfmt ")
	b = le.AppendUint32(b, 16)
	b = le.AppendUint16(b, format)
	b = le.AppendUint16(b, channels)
	b = le.AppendUint32(b, rate)
	b = le.AppendUint32(b, rate*uint32(bits/8))
	b = le.AppendUint16(b, bits/8)
	b = le.AppendUint16(b, bits)
	if extra != "" {
		b = append(b, "LIST"...)
		b = le.AppendUint32(b, uint32(len(extra)))
		b = append(b, extra...)
		if len(extra)%2 == 1 {
			b = append(b, 0)
		}
	}
	b = append(b, "data"...)
	b = le.AppendUint32(b, uint32(dataSize))
	b = append(b, samples...)
	le.PutUint32(b[4:], uint32(len(b)-8))
	return b
}

func TestSpeechWAV(t *testing.T) {
	samples := []byte{1, 2, 3, 4, 5, 6}
	want := pcmWAV(samples, 24000)
	for name, in := range map[string][]byte{
		"plain":            engineWAV(1, 1, 24000, 16, "", samples, 6),
		"extra chunk":      engineWAV(1, 1, 24000, 16, "odd", samples, 6),
		"unset data size":  engineWAV(1, 1, 24000, 16, "", samples, 0xffffffff),
		"data size larger": engineWAV(1, 1, 24000, 16, "", samples, 100),
	} {
		got, err := SpeechWAV(in, 24000)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, in := range map[string][]byte{
		"float":    engineWAV(3, 1, 24000, 32, "", samples, 6),
		"stereo":   engineWAV(1, 2, 24000, 16, "", samples, 6),
		"rate":     engineWAV(1, 1, 16000, 16, "", samples, 6),
		"empty":    engineWAV(1, 1, 24000, 16, "", nil, 0),
		"not riff": []byte("hello world, this is not audio"),
		"short":    []byte("RIFF"),
	} {
		if _, err := SpeechWAV(in, 24000); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	got, _ := SpeechWAV(want, 24000)
	if !bytes.Equal(got, want) {
		t.Error("a canonical WAV does not come back unchanged")
	}
}

// serveArchive serves the file at p, counting requests.
func serveArchive(t *testing.T, p string) (*httptest.Server, *int) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	n := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*n++
		mu.Unlock()
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, n
}

func TestFetchAndVerifySpeech(t *testing.T) {
	m := testSpeechModel()
	srv, asked := serveArchive(t, testSpeechArchive)
	dir := filepath.Join(t.TempDir(), "models")
	f := Fetcher{AllowHost: loopbackOnly(srv)}
	root, err := f.FetchSpeechFrom(context.Background(), m, dir, srv.URL+"/a.tar.bz2")
	if err != nil {
		t.Fatal(err)
	}
	if root != SpeechDir(dir, m) {
		t.Fatalf("root %s", root)
	}
	files, err := VerifySpeech(dir, m)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(files, testSpeechManifest()) {
		t.Fatalf("%+v", files)
	}
	if _, err := os.Lstat(filepath.Join(root, "._model.onnx")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("._model.onnx: %v", err)
	}
	for _, d := range []string{"espeak-ng-data/lang", "espeak-ng-data/empty", "espeak-ng-data/voices/!v"} {
		if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(d))); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v", d, err)
		}
	}
	if fi, _ := os.Stat(SpeechPath(dir, m, "model.onnx")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", fi.Mode())
	}
	// Nothing is left beside the model: no archive, no temp directory.
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("%v", entries)
	}
	// A verified model is not downloaded again.
	if _, err := f.FetchSpeechFrom(context.Background(), m, dir, srv.URL+"/a.tar.bz2"); err != nil || *asked != 1 {
		t.Fatalf("%v, %d downloads", err, *asked)
	}
	// A changed file fails the check, and the next fetch replaces the model.
	if err := os.WriteFile(filepath.Join(root, "tokens.txt"), []byte("a 9\nb 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySpeech(dir, m); !errors.Is(err, ErrModel) {
		t.Fatalf("changed file: %v", err)
	}
	if _, err := f.FetchSpeechFrom(context.Background(), m, dir, srv.URL+"/a.tar.bz2"); err != nil || *asked != 2 {
		t.Fatalf("%v, %d downloads", err, *asked)
	}
	if _, err := VerifySpeech(dir, m); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("the old model is left: %v", entries)
	}
	// An archive of other bytes is refused, and the model there kept.
	other := m
	other.ArchiveSHA256 = strings.Repeat("0", 64)
	if _, err := f.FetchSpeechFrom(context.Background(), other, filepath.Join(t.TempDir(), "m3"), srv.URL+"/a.tar.bz2"); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("wrong sha256: %v", err)
	}
	// An unpinned model is never fetched.
	u := m
	u.ArchiveSHA256 = Unverified
	if _, err := f.FetchSpeechFrom(context.Background(), u, dir, srv.URL); !errors.Is(err, ErrUnpinned) {
		t.Fatalf("unpinned: %v", err)
	}
	// The real source is GitHub's release hosts only.
	if _, err := (Fetcher{}).FetchSpeechFrom(context.Background(), m, filepath.Join(t.TempDir(), "m2"), srv.URL); err == nil || !strings.Contains(err.Error(), "not a GitHub release host") {
		t.Fatalf("loopback source: %v", err)
	}
}

func TestGitHubReleaseHost(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/x.tar.bz2":   true,
		"https://release-assets.githubusercontent.com/github-production-release-asset/1": true,
		"https://objects.githubusercontent.com/github-production-release-asset-2e65be/1": true,
		"https://github.com:443/x":                   true,
		"http://github.com/x":                        false,
		"https://github.com:8443/x":                  false,
		"https://user@github.com/x":                  false,
		"https://raw.githubusercontent.com/x/y/z":    false,
		"https://github.com.evil.test/x":             false,
		"https://evil.test/github.com":               false,
		"https://huggingface.co/x/resolve/abc/model": false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := GitHubReleaseHost(u); got != want {
			t.Errorf("%s: %v", raw, got)
		}
	}
}

// tarEntry is one entry of a test archive.
type tarEntry struct {
	name, body string
	typ        byte
	link       string
}

func testTar(t *testing.T, entries []tarEntry) *tar.Reader {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Linkname: e.link, ModTime: time.Unix(1700000000, 0)}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := w.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return tar.NewReader(&b)
}

// goodTar is a complete tiny-kokoro archive's entries.
func goodTar() []tarEntry {
	var out []tarEntry
	for _, f := range testSpeechManifest() {
		out = append(out, tarEntry{name: "tiny-kokoro/" + f.Path, body: testSpeechFiles[f.Path], typ: tar.TypeReg})
	}
	return out
}

// privateTemp is a private (0700) temporary directory, as the one a fetch
// unpacks into.
func privateTemp(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "into")
	if err := PrivateDir(d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestUnpackSpeechTar(t *testing.T) {
	m := testSpeechModel()
	files, err := unpackSpeechTar(testTar(t, append([]tarEntry{{name: "./tiny-kokoro/", typ: tar.TypeDir}, {name: "tiny-kokoro/x/.hidden/y", body: "z", typ: tar.TypeReg}}, goodTar()...)), privateTemp(t), m)
	if err != nil || !slices.Equal(files, testSpeechManifest()) {
		t.Fatalf("%v %+v", err, files)
	}
	for name, tc := range map[string]struct {
		extra []tarEntry
		drop  string
		want  string
	}{
		"outside the top":   {extra: []tarEntry{{name: "other/x", body: "x", typ: tar.TypeReg}}, want: "not below"},
		"a file at the top": {extra: []tarEntry{{name: "tiny-kokoro", body: "x", typ: tar.TypeReg}}, want: "not a plain relative path"},
		"dotdot":            {extra: []tarEntry{{name: "tiny-kokoro/../x", body: "x", typ: tar.TypeReg}}, want: "not a plain relative path"},
		"absolute":          {extra: []tarEntry{{name: "/tiny-kokoro/x", body: "x", typ: tar.TypeReg}}, want: "not below"},
		"double slash":      {extra: []tarEntry{{name: "tiny-kokoro/a//x", body: "x", typ: tar.TypeReg}}, want: "not a plain relative path"},
		"control character": {extra: []tarEntry{{name: "tiny-kokoro/a\tx", body: "x", typ: tar.TypeReg}}, want: "not a plain relative path"},
		"symlink":           {extra: []tarEntry{{name: "tiny-kokoro/l", typ: tar.TypeSymlink, link: "/etc/passwd"}}, want: "not a regular file or a directory"},
		"hard link":         {extra: []tarEntry{{name: "tiny-kokoro/l", typ: tar.TypeLink, link: "tiny-kokoro/model.onnx"}}, want: "not a regular file or a directory"},
		"fifo":              {extra: []tarEntry{{name: "tiny-kokoro/f", typ: tar.TypeFifo}}, want: "not a regular file or a directory"},
		"twice":             {extra: []tarEntry{{name: "tiny-kokoro/tokens.txt", body: "again", typ: tar.TypeReg}}, want: "exists"},
		"file then dir":     {extra: []tarEntry{{name: "tiny-kokoro/tokens.txt/x", body: "x", typ: tar.TypeReg}}, want: "tokens.txt"},
		"no voices":         {drop: "voices.bin", want: "no voices.bin"},
		"no phondata":       {drop: "espeak-ng-data/phondata", want: "no espeak-ng-data/phondata"},
	} {
		t.Run(name, func(t *testing.T) {
			var es []tarEntry
			for _, e := range goodTar() {
				if e.name != "tiny-kokoro/"+tc.drop {
					es = append(es, e)
				}
			}
			es = append(es, tc.extra...)
			into := privateTemp(t)
			_, err := unpackSpeechTar(testTar(t, es), into, m)
			if !errors.Is(err, ErrModel) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if _, err := os.Lstat(filepath.Join(into, "l")); err == nil {
				t.Fatal("a link was made")
			}
		})
	}
}

func TestFetchSpeechRefusesABadArchive(t *testing.T) {
	// The pinned archive whose top directory is not the one the table
	// names: nothing lands.
	m := testSpeechModel()
	m.ArchiveRoot = "other"
	srv, _ := serveArchive(t, testSpeechArchive)
	dir := filepath.Join(t.TempDir(), "models")
	if _, err := (Fetcher{AllowHost: loopbackOnly(srv)}).FetchSpeechFrom(context.Background(), m, dir, srv.URL); !errors.Is(err, ErrModel) || !strings.Contains(err.Error(), "not below other/") {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestVerifySpeechRefusesUnsafeLayouts(t *testing.T) {
	m := testSpeechModel()
	t.Run("missing", func(t *testing.T) {
		if _, err := VerifySpeech(t.TempDir(), m); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		dir := t.TempDir()
		placeSpeech(t, dir, m)
		if err := os.Remove(SpeechPath(dir, m, "espeak-ng-data/en_dict")); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifySpeech(dir, m); !errors.Is(err, ErrModel) || !strings.Contains(err.Error(), "missing") {
			t.Fatal(err)
		}
	})
	for name, tc := range map[string]struct {
		mut  func(t *testing.T, dir string)
		want string
	}{
		"symlinked directory": {func(t *testing.T, dir string) {
			data := filepath.Join(SpeechDir(dir, m), "espeak-ng-data")
			moved := filepath.Join(dir, "elsewhere")
			if err := os.Rename(data, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, data); err != nil {
				t.Fatal(err)
			}
		}, "symbolic link"},
		"open directory": {func(t *testing.T, dir string) {
			if err := os.Chmod(SpeechDir(dir, m), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "other users"},
		"open subdirectory": {func(t *testing.T, dir string) {
			if err := os.Chmod(SpeechPath(dir, m, "espeak-ng-data/lang"), 0o777); err != nil {
				t.Fatal(err)
			}
		}, "other users"},
		"writable file": {func(t *testing.T, dir string) {
			if err := os.Chmod(SpeechPath(dir, m, "model.onnx"), 0o666); err != nil {
				t.Fatal(err)
			}
		}, "another user"},
		"an extra file": {func(t *testing.T, dir string) {
			if err := os.WriteFile(SpeechPath(dir, m, "espeak-ng-data/lang/gmw/en-GB"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "not among the files"},
		"an extra link": {func(t *testing.T, dir string) {
			if err := os.Symlink("/etc/passwd", SpeechPath(dir, m, "espeak-ng-data/fr_dict")); err != nil {
				t.Fatal(err)
			}
		}, "symbolic link"},
		"a file made a link": {func(t *testing.T, dir string) {
			p, moved := SpeechPath(dir, m, "tokens.txt"), filepath.Join(dir, "tokens.txt")
			if err := os.Rename(p, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, p); err != nil {
				t.Fatal(err)
			}
		}, "symbolic link"},
		"no manifest": {func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(SpeechDir(dir, m), speechManifest)); err != nil {
				t.Fatal(err)
			}
		}, "did not unpack it"},
		"writable manifest": {func(t *testing.T, dir string) {
			if err := os.Chmod(filepath.Join(SpeechDir(dir, m), speechManifest), 0o622); err != nil {
				t.Fatal(err)
			}
		}, "another user"},
		"another archive's manifest": {func(t *testing.T, dir string) {
			o := m
			o.ArchiveSHA256 = strings.Repeat("e", 64)
			p := filepath.Join(SpeechDir(dir, m), speechManifest)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := writeSpeechManifest(SpeechDir(dir, m), o, testSpeechManifest()); err != nil {
				t.Fatal(err)
			}
		}, "another archive"},
		"a manifest without a role": {func(t *testing.T, dir string) {
			files := slices.DeleteFunc(testSpeechManifest(), func(f SpeechFile) bool { return f.Path == "voices.bin" })
			if err := os.Remove(filepath.Join(SpeechDir(dir, m), speechManifest)); err != nil {
				t.Fatal(err)
			}
			if err := writeSpeechManifest(SpeechDir(dir, m), m, files); err != nil {
				t.Fatal(err)
			}
		}, "no voices.bin"},
		"a manifest with a dotdot path": {func(t *testing.T, dir string) {
			files := append(testSpeechManifest(), SpeechFile{Path: "../x", Size: 1, SHA256: strings.Repeat("a", 64)})
			if err := os.Remove(filepath.Join(SpeechDir(dir, m), speechManifest)); err != nil {
				t.Fatal(err)
			}
			if err := writeSpeechManifest(SpeechDir(dir, m), m, files); err != nil {
				t.Fatal(err)
			}
		}, "not a plain path"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			placeSpeech(t, dir, m)
			if _, err := VerifySpeech(dir, m); err != nil {
				t.Fatal(err)
			}
			tc.mut(t, dir)
			if _, err := VerifySpeech(dir, m); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

var (
	fakeSherpaOnce sync.Once
	fakeSherpaBin  string
	fakeSherpaErr  error
)

// fakeSherpa builds tools/test/fakesherpa once per test binary, into a
// private directory (CheckServer refuses one others can write).
func fakeSherpa(t *testing.T) string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go is not on PATH; the fake sherpa-onnx cannot be built")
	}
	fakeSherpaOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakesherpa-")
		if err != nil {
			fakeSherpaErr = err
			return
		}
		fakeSherpaBin = filepath.Join(dir, "sherpa-onnx-offline-tts")
		out, err := exec.Command(gobin, "build", "-o", fakeSherpaBin, "../../tools/test/fakesherpa").CombinedOutput()
		if err != nil {
			fakeSherpaErr = fmt.Errorf("%v: %s", err, out)
		}
	})
	if fakeSherpaErr != nil {
		t.Fatal(fakeSherpaErr)
	}
	return fakeSherpaBin
}

// testSpeaker is a Speaker of testSpeechModel with its files in place and
// the fake engine.
func testSpeaker(t *testing.T) (*Speaker, *logs) {
	m := testSpeechModel()
	dir := filepath.Join(t.TempDir(), "models")
	if err := PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	placeSpeech(t, dir, m)
	l := &logs{}
	return &Speaker{Model: m, ModelDir: dir, Server: fakeSherpa(t), Voice: "bf_emma", Speed: 1, Threads: 2, MaxChars: 200, Log: l.f}, l
}

func noSpeakTemp(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), speakTempPrefix) {
			t.Fatalf("%s left behind", e.Name())
		}
	}
}

func TestSpeakerSpeaks(t *testing.T) {
	s, _ := testSpeaker(t)
	if !s.Ready() {
		_, err := s.Check()
		t.Fatalf("not ready: %v", err)
	}
	wav, err := s.Speak(context.Background(), "  Hello\nthere.  ", "")
	if err != nil {
		t.Fatal(err)
	}
	// The fake writes 100 samples per byte of the text it got, after a
	// LIST chunk; lever answers them in its own header.
	if want := pcmWAV(make([]byte, 200*len("Hello there.")), 24000); !bytes.Equal(wav, want) {
		t.Fatalf("got %d bytes, want %d", len(wav), len(want))
	}
	noSpeakTemp(t, s.ModelDir)
	// The arguments the engine got: the voice's id, the cleaned text last.
	wav, err = s.Speak(context.Background(), "ARGV now", "af")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimRight(string(wav[WAVHeaderLen:]), "\x00"), "\x00")
	if args[len(args)-1] != "ARGV now" || !slices.Contains(args, "--sid=0") || !slices.Contains(args, "--num-threads=2") {
		t.Fatalf("args %q", args)
	}
	noSpeakTemp(t, s.ModelDir)
}

func TestSpeakerRefusals(t *testing.T) {
	s, l := testSpeaker(t)
	ctx := context.Background()
	if _, err := s.Speak(ctx, "hi", "nobody"); !errors.Is(err, ErrSpeakNotSent) {
		t.Fatalf("unknown voice: %v", err)
	}
	if _, err := s.Speak(ctx, strings.Repeat("a", 201), ""); !errors.Is(err, ErrSpeakNotSent) || !errors.Is(err, ErrBadText) {
		t.Fatalf("too long: %v", err)
	}
	if _, err := s.Speak(ctx, "please FAIL secret words", ""); !errors.Is(err, ErrSpeakFailed) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("engine failure: %v", err)
	}
	if _, err := s.Speak(ctx, "FLOAT", ""); !errors.Is(err, ErrSpeakFailed) {
		t.Fatalf("float output: %v", err)
	}
	s.MaxWait = 200 * time.Millisecond
	start := time.Now()
	if _, err := s.Speak(ctx, "SLEEP", ""); !errors.Is(err, ErrSpeakFailed) || time.Since(start) > 10*time.Second {
		t.Fatalf("slow engine: %v after %s", err, time.Since(start))
	}
	noSpeakTemp(t, s.ModelDir)
	for _, line := range l.lines {
		if strings.Contains(line, "secret") {
			t.Fatalf("the text reached the log: %q", line)
		}
	}
}

func TestSpeakerChecksTheModelBeforeEveryRun(t *testing.T) {
	s, _ := testSpeaker(t)
	if _, err := s.Speak(context.Background(), "one", ""); err != nil {
		t.Fatal(err)
	}
	p := SpeechPath(s.ModelDir, s.Model, "voices.bin")
	if err := os.WriteFile(p, []byte("voice bytez"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Speak(context.Background(), "two", ""); !errors.Is(err, ErrSpeakNotSent) || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("changed model: %v", err)
	}
	if s.Ready() {
		t.Fatal("ready with a changed model")
	}
	if err := os.WriteFile(p, []byte(testSpeechFiles["voices.bin"]), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Speak(context.Background(), "three", ""); err != nil {
		t.Fatalf("restored model: %v", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Check(); err == nil || !strings.Contains(err.Error(), "voices.bin is missing") {
		t.Fatalf("missing file: %v", err)
	}
	// The failure is remembered for the same layout, and a file that
	// appears is checked.
	if s.bad == "" {
		t.Fatal("a missing file left no stamp")
	}
	if _, err := s.Check(); err == nil {
		t.Fatal("missing file, second check")
	}
	if err := os.WriteFile(p, []byte(testSpeechFiles["voices.bin"]), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Check(); err != nil {
		t.Fatalf("file back: %v", err)
	}
}

func TestSpeakerRefusesAReplacedProgram(t *testing.T) {
	s, _ := testSpeaker(t)
	dir := t.TempDir()
	link := filepath.Join(dir, "tts")
	if err := os.Symlink(s.Server, link); err != nil {
		t.Fatal(err)
	}
	s.Server = link
	if _, err := s.Check(); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other")
	b, _ := os.ReadFile(fakeSherpa(t))
	if err := os.WriteFile(other, b, 0o700); err != nil {
		t.Fatal(err)
	}
	os.Remove(link)
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Check(); err == nil || !strings.Contains(err.Error(), "now resolves") {
		t.Fatalf("replaced program: %v", err)
	}
}

func TestRemoveStaleSpeech(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{".speak-1", ".speak-2"} {
		if err := os.MkdirAll(filepath.Join(dir, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(dir, "model.bin")
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A fresh one may be another tool's run: kept.
	RemoveStaleSpeech(dir, time.Now())
	if _, err := os.Stat(filepath.Join(dir, ".speak-1")); err != nil {
		t.Fatal("a fresh output directory was removed")
	}
	RemoveStaleSpeech(dir, time.Now().Add(time.Hour))
	noSpeakTemp(t, dir)
	if _, err := os.Stat(keep); err != nil {
		t.Fatal(err)
	}
}

// fakeEngine is a SpeechEngine for the socket tests.
type fakeEngine struct {
	ready bool
	err   error
	got   []string
	mu    sync.Mutex
}

func (f *fakeEngine) Ready() bool { return f.ready }
func (f *fakeEngine) Speak(ctx context.Context, text, voice string) ([]byte, error) {
	f.mu.Lock()
	f.got = append(f.got, voice+":"+text)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return pcmWAV([]byte{1, 2}, 24000), nil
}

func TestSpeakSocket(t *testing.T) {
	m := testSpeechModel()
	sock := filepath.Join(shortDir(t), "run", "s.sock")
	ln, err := ListenSpeak(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(sock); fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", fi.Mode())
	}
	eng := &fakeEngine{ready: true}
	srv := NewSpeakServer(&SpeakHandler{Engine: eng, Model: m, Voice: "bf_emma", MaxChars: 50})
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	c := &SpeakClient{Socket: sock}
	h, ok := c.Available()
	if !ok || h.Model != "tiny-kokoro" || h.Voice != "bf_emma" || h.MaxChars != 50 || len(h.Voices) != 2 || h.Voices[1] != (SpeakVoice{"bf_emma", "en-GB"}) {
		t.Fatalf("%+v %v", h, ok)
	}
	wav, err := c.Speak(context.Background(), "Hello.", "")
	if err != nil || !bytes.Equal(wav, pcmWAV([]byte{1, 2}, 24000)) {
		t.Fatalf("%v", err)
	}
	if _, err := c.Speak(context.Background(), "Hello.", "af"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(eng.got, []string{"bf_emma:Hello.", "af:Hello."}) {
		t.Fatalf("engine got %q", eng.got)
	}
	for name, tc := range map[string]struct{ text, voice string }{
		"bad voice": {"Hello.", "zz"},
		"too long":  {strings.Repeat("a", 51), ""},
		"empty":     {"  ", ""},
	} {
		if _, err := c.Speak(context.Background(), tc.text, tc.voice); !errors.Is(err, ErrSpeakNotSent) {
			t.Errorf("%s: %v", name, err)
		}
	}
	eng.err = fmt.Errorf("%w: not ready", ErrSpeakNotSent)
	if _, err := c.Speak(context.Background(), "Hi.", ""); !errors.Is(err, ErrSpeakNotSent) {
		t.Errorf("not sent: %v", err)
	}
	eng.err = ErrSpeakFailed
	if _, err := c.Speak(context.Background(), "Hi.", ""); err == nil || errors.Is(err, ErrSpeakNotSent) {
		t.Errorf("failed: %v", err)
	}
	// A second tool on the same socket is refused.
	if _, err := ListenSpeak(sock); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("second listener: %v", err)
	}
}

func TestSpeakClientWithNoTool(t *testing.T) {
	dir := shortDir(t)
	c := &SpeakClient{Socket: filepath.Join(dir, "none.sock")}
	if _, ok := c.Available(); ok {
		t.Fatal("available with no tool")
	}
	if _, err := c.Speak(context.Background(), "Hi.", ""); !errors.Is(err, ErrSpeakNotSent) {
		t.Fatalf("%v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Speak(context.Background(), "Hi.", ""); !errors.Is(err, ErrSpeakNotSent) || !strings.Contains(err.Error(), "not a private directory") {
		t.Fatalf("open directory: %v", err)
	}
}
