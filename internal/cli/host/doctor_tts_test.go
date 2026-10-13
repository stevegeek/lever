package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/voice"
)

// testSpeechArchive is the voice package's tiny Kokoro archive (top
// directory tiny-kokoro/, 10 files).
const testSpeechArchive = "../../voice/testdata/tiny-kokoro.tar.bz2"

// pinTestSpeech swaps lever's speech model table for one model named
// kokoro-en-v0_19 pinned to testSpeechArchive, for the test's length.
func pinTestSpeech(t *testing.T) voice.SpeechModel {
	t.Helper()
	b, err := os.ReadFile(testSpeechArchive)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	m := voice.SpeechModel{Name: voice.DefaultSpeechModel, Kind: voice.KindKokoro,
		ArchiveURL:  "https://github.com/someone/tiny/releases/download/models/tiny-kokoro.tar.bz2",
		ArchiveSize: int64(len(b)), ArchiveSHA256: hex.EncodeToString(sum[:]), ArchiveRoot: "tiny-kokoro",
		ModelFile: "model.onnx", TokensFile: "tokens.txt", VoicesFile: "voices.bin", DataDir: "espeak-ng-data",
		Voices: []voice.SpeechVoice{{Name: "bf_emma", SID: 7, Lang: "en-GB"}}, DefaultVoice: "bf_emma", SampleRate: 24000}
	old := voice.SpeechModels
	voice.SpeechModels = []voice.SpeechModel{m}
	t.Cleanup(func() { voice.SpeechModels = old })
	return m
}

// archiveFetch is a fetch of a speech model from a loopback server that
// serves testSpeechArchive.
func archiveFetch(t *testing.T) func(context.Context, voice.SpeechModel, string) (string, error) {
	t.Helper()
	b, err := os.ReadFile(testSpeechArchive)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(b) }))
	t.Cleanup(srv.Close)
	return func(ctx context.Context, m voice.SpeechModel, dir string) (string, error) {
		f := voice.Fetcher{AllowHost: func(u *url.URL) bool { return "http://"+u.Host == srv.URL }}
		return f.FetchSpeechFrom(ctx, m, dir, srv.URL+"/tiny-kokoro.tar.bz2")
	}
}

// ttsDoctorApp is voiceDoctorApp with read_aloud: server, served by a
// lever-tool-tts entry whose models and socket are outside the tree.
func ttsDoctorApp(t *testing.T) *config.App {
	t.Helper()
	app, _ := voiceDoctorApp(t)
	sock := filepath.Join(filepath.Dir(app.Remote.Voice.Socket), "t.sock")
	models := filepath.Join(t.TempDir(), "tts-models")
	app.Remote.Voice.ReadAloud, app.Remote.Voice.SpeakSocket = config.ReadAloudServer, sock
	app.Broker.Tools = append(app.Broker.Tools, config.Tool{Name: "tts", Backend: "127.0.0.1:3213", Command: []string{"/opt/lever-tool-tts",
		"-tree", app.Tree, "-models", models, "-server", "/opt/sherpa/sherpa-onnx-offline-tts", "-speak-socket", sock}})
	return app
}

type speechStub struct{ ready bool }

func (s *speechStub) Ready() bool { return s.ready }
func (s *speechStub) Speak(context.Context, string, string) ([]byte, error) {
	return nil, voice.ErrSpeakNotSent
}

func TestCheckTTSRowsAbsentWithoutATool(t *testing.T) {
	app, _ := voiceDoctorApp(t)
	if r := checkTTSTool(context.Background(), app); r.name != "" {
		t.Fatalf("a tool row %+v", r)
	}
	if r := checkTTSModel(app); r.name != "" {
		t.Fatalf("a model row %+v", r)
	}
	if r := checkVoice(app); !strings.Contains(r.detail, "read-aloud with the device's voices") {
		t.Fatalf("voice row %+v", r)
	}
}

func TestCheckTTSTool(t *testing.T) {
	app := ttsDoctorApp(t)
	if r := checkVoice(app); !r.ok || !strings.Contains(r.detail, "read-aloud with server voices through "+app.Remote.Voice.SpeakSocket) {
		t.Fatalf("voice row %+v", r)
	}
	if r := checkTTSTool(context.Background(), app); r.ok || !strings.Contains(r.detail, "not running") ||
		!strings.Contains(r.detail, "falls back to the device's voices") || !strings.Contains(r.fix, "tool-logs/tts.log") {
		t.Fatalf("no socket: %+v", r)
	}
	ln, err := voice.ListenSpeak(app.Remote.Voice.SpeakSocket)
	if err != nil {
		t.Fatal(err)
	}
	m := pinTestSpeech(t)
	stub := &speechStub{}
	srv := voice.NewSpeakServer(&voice.SpeakHandler{Engine: stub, Model: m, Voice: "bf_emma", MaxChars: 600})
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	if r := checkTTSTool(context.Background(), app); r.ok || !strings.Contains(r.detail, "not ready") || !strings.Contains(r.fix, "lever voice fetch kokoro-en-v0_19") {
		t.Fatalf("not ready: %+v", r)
	}
	stub.ready = true
	if r := checkTTSTool(context.Background(), app); !r.ok || !strings.Contains(r.detail, "ready: model kokoro-en-v0_19, voice bf_emma of 1, at most 600 characters") ||
		!strings.Contains(r.detail, "no port") {
		t.Fatalf("ready: %+v", r)
	}
	// A tts tool with read_aloud device: still checked, and said unused.
	app.Remote.Voice.ReadAloud = config.ReadAloudDevice
	if r := checkTTSTool(context.Background(), app); !r.ok || !strings.Contains(r.detail, "does not use it") {
		t.Fatalf("device: %+v", r)
	}
}

func TestCheckTTSModel(t *testing.T) {
	app := ttsDoctorApp(t)
	tool, _ := app.SpeechTool()
	if m, _ := voice.LookupSpeech(voice.DefaultSpeechModel); m.Pinned() != "" {
		if r := checkTTSModel(app); r.ok || !strings.Contains(r.detail, "not verified") {
			t.Fatalf("unverified table: %+v", r)
		}
	}
	m := pinTestSpeech(t)
	if r := checkTTSModel(app); r.ok || !strings.Contains(r.detail, "not downloaded") || !strings.Contains(r.fix, "lever voice fetch "+m.Name) {
		t.Fatalf("missing: %+v", r)
	}
	if _, err := archiveFetch(t)(context.Background(), m, tool.Models); err != nil {
		t.Fatal(err)
	}
	if r := checkTTSModel(app); !r.ok || !strings.Contains(r.detail, "10 files") || !strings.Contains(r.detail, m.ArchiveSHA256[:12]) {
		t.Fatalf("good: %+v", r)
	}
	if err := os.WriteFile(voice.SpeechPath(tool.Models, m, "tokens.txt"), []byte("a 9\nb 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := checkTTSModel(app); r.ok || !strings.Contains(r.detail, "sha256") {
		t.Fatalf("wrong file: %+v", r)
	}
	app.Broker.Tools[1].Command = append(app.Broker.Tools[1].Command, "-model", "tacotron")
	if r := checkTTSModel(app); r.ok || !strings.Contains(r.detail, "not in lever's speech model table") {
		t.Fatalf("unknown model: %+v", r)
	}
	app.Broker.Tools = app.Broker.Tools[:1]
	if r := checkTTSModel(app); !r.ok || !strings.Contains(r.detail, "not checked") {
		t.Fatalf("no tool: %+v", r)
	}
}

func TestRunSpeechFetch(t *testing.T) {
	app := ttsDoctorApp(t)
	_, st := voiceDoctorApp(t)
	tool, _ := app.SpeechTool()
	m := pinTestSpeech(t)
	fetch := archiveFetch(t)
	var out bytes.Buffer
	if err := runSpeechFetch(context.Background(), app, st, "", fetch, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := voice.VerifySpeech(tool.Models, m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "verified") || !strings.Contains(out.String(), "picks it up") {
		t.Fatalf("%s", out.String())
	}
	if r := checkTTSModel(app); !r.ok {
		t.Fatalf("doctor after the fetch: %+v", r)
	}
	if err := runSpeechFetch(context.Background(), app, st, "tacotron", fetch, &out); err == nil || !strings.Contains(err.Error(), "not in lever's pinned speech model table") {
		t.Fatalf("unknown model: %v", err)
	}
	// No tool: the state directory.
	app.Broker.Tools = app.Broker.Tools[:1]
	out.Reset()
	if err := runSpeechFetch(context.Background(), app, st, "", fetch, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := voice.VerifySpeech(st.VoiceModels(), m); err != nil || !strings.Contains(out.String(), "-models "+st.VoiceModels()) {
		t.Fatalf("%v %s", err, out.String())
	}
	// Unpinned: nothing is fetched.
	voice.SpeechModels[0].ArchiveSHA256 = voice.Unverified
	called := false
	nofetch := func(context.Context, voice.SpeechModel, string) (string, error) { called = true; return "", nil }
	if err := runSpeechFetch(context.Background(), app, st, "", nofetch, &out); err == nil || called || !strings.Contains(err.Error(), "not verified") {
		t.Fatalf("%v %v", err, called)
	}
}
