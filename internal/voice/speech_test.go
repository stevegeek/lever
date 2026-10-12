package voice

import (
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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// testSpeechFiles is the content of a tiny Kokoro model's files.
var testSpeechFiles = map[string]string{
	"model.onnx":                  "onnx bytes",
	"voices.bin":                  "voice bytes",
	"tokens.txt":                  "a 1\nb 2\n",
	"espeak-ng-data/phontab":      "phontab",
	"espeak-ng-data/lang/gmw/en":  "name english",
	"espeak-ng-data/en_dict":      "dict",
	"espeak-ng-data/intonations":  "into",
	"espeak-ng-data/phondata":     "phondata",
	"espeak-ng-data/phonindex":    "phonindex",
	"espeak-ng-data/voices/!v/xx": "variant",
}

// testSpeechModel is a pinned Kokoro model of testSpeechFiles.
func testSpeechModel() SpeechModel {
	m := SpeechModel{
		Name: "tiny-kokoro", Kind: KindKokoro, Repo: "someone/tiny-kokoro", Revision: strings.Repeat("b", 40),
		ModelFile: "model.onnx", TokensFile: "tokens.txt", VoicesFile: "voices.bin", DataDir: "espeak-ng-data",
		Voices:       []SpeechVoice{{"af", 0, "en-US"}, {"bf_emma", 7, "en-GB"}},
		DefaultVoice: "bf_emma", SampleRate: 24000,
	}
	var paths []string
	for p := range testSpeechFiles {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		sum := sha256.Sum256([]byte(testSpeechFiles[p]))
		m.Files = append(m.Files, SpeechFile{Path: p, Size: int64(len(testSpeechFiles[p])), SHA256: hex.EncodeToString(sum[:])})
	}
	return m
}

// placeSpeech writes m's files into dir as a fetch would.
func placeSpeech(t *testing.T, dir string, m SpeechModel) {
	t.Helper()
	for _, f := range m.Files {
		sub, err := privateDirs(SpeechDir(dir, m), filepath.ToSlash(filepath.Dir(f.Path)), true)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, filepath.Base(f.Path)), []byte(testSpeechFiles[f.Path]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestShippedSpeechModelsAreRefusedUntilPinned(t *testing.T) {
	for _, name := range []string{"kokoro-en-v0_19", "piper-en_GB-alba-medium"} {
		m, ok := LookupSpeech(name)
		if !ok {
			t.Fatalf("%s is not in the table", name)
		}
		if _, ok := m.Voice(m.DefaultVoice); !ok {
			t.Errorf("%s: default voice %q is not one of its voices", name, m.DefaultVoice)
		}
		// Placeholder values: the model runs only once pinned for real.
		if m.Pinned() == "" && slices.ContainsFunc(m.Files, func(f SpeechFile) bool { return f.SHA256 == Unverified }) {
			t.Errorf("%s: Pinned accepts an unverified file", name)
		}
	}
	k, _ := LookupSpeech(DefaultSpeechModel)
	for _, v := range []string{"bf_emma", "bm_george"} {
		if _, ok := k.Voice(v); !ok {
			t.Errorf("the default model has no voice %s", v)
		}
	}
}

func TestSpeechModelPinned(t *testing.T) {
	if why := testSpeechModel().Pinned(); why != "" {
		t.Fatal(why)
	}
	for name, mut := range map[string]func(m *SpeechModel){
		"unverified revision": func(m *SpeechModel) { m.Revision = Unverified },
		"short revision":      func(m *SpeechModel) { m.Revision = "abc" },
		"unverified file":     func(m *SpeechModel) { m.Files[0].SHA256 = Unverified },
		"zero size":           func(m *SpeechModel) { m.Files[1].Size = 0 },
		"dotdot path":         func(m *SpeechModel) { m.Files[0].Path = "../x" },
		"absolute path":       func(m *SpeechModel) { m.Files[0].Path = "/x" },
		"dot component":       func(m *SpeechModel) { m.Files[0].Path = "espeak-ng-data/.x" },
		"duplicate":           func(m *SpeechModel) { m.Files[1].Path = m.Files[0].Path },
		"no files":            func(m *SpeechModel) { m.Files = nil },
		"no voices file":      func(m *SpeechModel) { m.VoicesFile = "nope.bin" },
		"no data dir":         func(m *SpeechModel) { m.DataDir = "nodata" },
		"bad default":         func(m *SpeechModel) { m.DefaultVoice = "zz" },
		"bad kind":            func(m *SpeechModel) { m.Kind = "tacotron" },
		"bad repo":            func(m *SpeechModel) { m.Repo = "a/b/c" },
		"bad rate":            func(m *SpeechModel) { m.SampleRate = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			m := testSpeechModel()
			m.Files = slices.Clone(m.Files)
			mut(&m)
			if m.Pinned() == "" {
				t.Fatal("accepted")
			}
		})
	}
}

func TestSpeechFileURL(t *testing.T) {
	m := testSpeechModel()
	got := m.FileURL(SpeechFile{Path: "espeak-ng-data/voices/!v/a b"})
	want := "https://huggingface.co/someone/tiny-kokoro/resolve/" + strings.Repeat("b", 40) + "/espeak-ng-data/voices/%21v/a%20b"
	if got != want {
		t.Fatalf("%s", got)
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
		"--kokoro-length-scale=0.800", "--num-threads=2", "--sid=7", "--output-filename=/m/.speak-1/speech.wav", "Hello.",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
	m.Kind, m.ModelFile, m.VoicesFile = KindVITS, "alba.onnx", ""
	got = SherpaArgs(m, "/m", SpeechVoice{"alba", 0, "en-GB"}, 1, 1, "/o.wav", "Hi.")
	want = []string{"--vits-model=/m/tiny-kokoro/alba.onnx", "--vits-tokens=/m/tiny-kokoro/tokens.txt",
		"--vits-data-dir=/m/tiny-kokoro/espeak-ng-data", "--vits-length-scale=1.000", "--num-threads=1", "--sid=0", "--output-filename=/o.wav", "Hi."}
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

func TestFetchAndVerifySpeech(t *testing.T) {
	m := testSpeechModel()
	var mu sync.Mutex
	asked := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		mu.Lock()
		asked[p]++
		mu.Unlock()
		body, ok := testSpeechFiles[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	dir := filepath.Join(t.TempDir(), "models")
	src := func(f SpeechFile) string { return srv.URL + "/" + f.Path }
	f := Fetcher{AllowHost: loopbackOnly(srv)}
	root, err := f.FetchSpeechFrom(context.Background(), m, dir, src)
	if err != nil {
		t.Fatal(err)
	}
	if root != SpeechDir(dir, m) {
		t.Fatalf("root %s", root)
	}
	if err := VerifySpeech(dir, m); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(root, "espeak-ng-data", "lang")); fi.Mode().Perm() != 0o700 {
		t.Fatalf("subdirectory mode %v", fi.Mode())
	}
	// Verified files are not downloaded again.
	if _, err := f.FetchSpeechFrom(context.Background(), m, dir, src); err != nil {
		t.Fatal(err)
	}
	for p, n := range asked {
		if n != 1 {
			t.Errorf("%s fetched %d times", p, n)
		}
	}
	// A changed file fails the check, and the next fetch replaces it.
	if err := os.WriteFile(filepath.Join(root, "tokens.txt"), []byte("a 9\nb 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySpeech(dir, m); !errors.Is(err, ErrModel) {
		t.Fatalf("changed file: %v", err)
	}
	if _, err := f.FetchSpeechFrom(context.Background(), m, dir, src); err != nil {
		t.Fatal(err)
	}
	if err := VerifySpeech(dir, m); err != nil {
		t.Fatal(err)
	}
	// An unpinned model is never fetched.
	u := m
	u.Revision = Unverified
	if _, err := f.FetchSpeechFrom(context.Background(), u, dir, src); !errors.Is(err, ErrUnpinned) {
		t.Fatalf("unpinned: %v", err)
	}
	// The real source is Hugging Face only.
	if _, err := (Fetcher{}).FetchSpeechFrom(context.Background(), m, filepath.Join(t.TempDir(), "m2"), src); err == nil || !strings.Contains(err.Error(), "not a Hugging Face host") {
		t.Fatalf("loopback source: %v", err)
	}
}

func TestVerifySpeechRefusesUnsafeLayouts(t *testing.T) {
	m := testSpeechModel()
	t.Run("missing", func(t *testing.T) {
		if err := VerifySpeech(t.TempDir(), m); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	})
	t.Run("symlinked directory", func(t *testing.T) {
		dir := t.TempDir()
		placeSpeech(t, dir, m)
		data := filepath.Join(SpeechDir(dir, m), "espeak-ng-data")
		moved := filepath.Join(dir, "elsewhere")
		if err := os.Rename(data, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, data); err != nil {
			t.Fatal(err)
		}
		if err := VerifySpeech(dir, m); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatal(err)
		}
	})
	t.Run("open directory", func(t *testing.T) {
		dir := t.TempDir()
		placeSpeech(t, dir, m)
		if err := os.Chmod(SpeechDir(dir, m), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := VerifySpeech(dir, m); err == nil || !strings.Contains(err.Error(), "other users") {
			t.Fatal(err)
		}
	})
	t.Run("writable file", func(t *testing.T) {
		dir := t.TempDir()
		placeSpeech(t, dir, m)
		if err := os.Chmod(SpeechPath(dir, m, "model.onnx"), 0o666); err != nil {
			t.Fatal(err)
		}
		if err := VerifySpeech(dir, m); !errors.Is(err, ErrModel) {
			t.Fatal(err)
		}
	})
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
	if _, err := s.Check(); err == nil || !strings.Contains(err.Error(), "not downloaded") {
		t.Fatalf("missing file: %v", err)
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
	RemoveStaleSpeech(dir)
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
