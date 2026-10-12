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
	"sync/atomic"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/voice"
)

// pinTestModel swaps lever's model table for one pinned model named
// large-v3-turbo whose file is content, for the test's length.
func pinTestModel(t *testing.T, content string) voice.Model {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	m := voice.Model{Name: voice.DefaultModel, File: "ggml-test.bin", Revision: strings.Repeat("b", 40), Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}
	old := voice.Models
	voice.Models = []voice.Model{m}
	t.Cleanup(func() { voice.Models = old })
	return m
}

// voiceDoctorApp is filesApp with voice on, served by a lever-tool-whisper
// entry whose models directory and socket are outside the tree.
func voiceDoctorApp(t *testing.T) (*config.App, state.State) {
	t.Helper()
	app, st := filesApp(t, false)
	// Under /tmp: a long TMPDIR (macOS) must not push the socket path over
	// the Unix socket limit.
	run, err := os.MkdirTemp("/tmp", "dv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	sock := filepath.Join(run, "s", "d.sock")
	models := filepath.Join(t.TempDir(), "models")
	app.Remote.Voice = config.Voice{Enabled: true, Socket: sock}
	app.Broker.Tools = []config.Tool{{Name: "whisper", Backend: "127.0.0.1:3212", Command: []string{"/opt/lever-tool-whisper",
		"-tree", app.Tree, "-models", models, "-server", "/opt/w/whisper-server", "-whisper-port", "8448", "-dictate-socket", sock}}}
	return app, st
}

func TestCheckVoiceOff(t *testing.T) {
	app, _ := filesApp(t, false)
	if r := checkVoice(app); !r.ok || !strings.HasPrefix(r.detail, "off") {
		t.Fatalf("%+v", r)
	}
	if r := checkVoiceModel(app); r.name != "" {
		t.Fatalf("off: a model row %+v", r)
	}
	if r := checkVoiceTool(context.Background(), app); r.name != "" {
		t.Fatalf("off: a tool row %+v", r)
	}
}

// remote.voice off with a whisper tool configured (agents only): the tool
// and model rows still run, against the tool's own -dictate-socket; the
// voice row says off.
func TestCheckVoiceRowsWithVoiceOff(t *testing.T) {
	app, _ := voiceDoctorApp(t)
	sock := app.Remote.Voice.Socket
	app.Remote.Voice = config.Voice{}
	if r := checkVoice(app); !r.ok || !strings.HasPrefix(r.detail, "off") {
		t.Fatalf("voice row: %+v", r)
	}
	m := pinTestModel(t, "weights")
	r := checkVoiceModel(app)
	if r.name != "voice model" || r.ok || !strings.Contains(r.detail, "agents cannot transcribe") || !strings.Contains(r.fix, "lever voice fetch "+m.Name) {
		t.Fatalf("model row: %+v", r)
	}
	r = checkVoiceTool(context.Background(), app)
	if r.name != "voice tool" || r.ok || !strings.Contains(r.detail, sock) || !strings.Contains(r.detail, "agents cannot transcribe") ||
		!strings.Contains(r.fix, "tool-logs/whisper.log") {
		t.Fatalf("tool row, not running: %+v", r)
	}
	ln, err := voice.ListenDictate(sock)
	if err != nil {
		t.Fatal(err)
	}
	stub := &readyStub{}
	stub.ready.Store(true)
	srv := voice.NewDictateServer(&voice.DictateHandler{Svc: stub, Queue: &voice.Queue{}, Model: "large-v3-turbo", MaxSeconds: 300})
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	if r := checkVoiceTool(context.Background(), app); !r.ok || !strings.Contains(r.detail, "agent clips at most 120 s") || !strings.Contains(r.detail, "for agents; remote.voice is off") {
		t.Fatalf("tool row, ready: %+v", r)
	}
}

func TestCheckVoiceConfigAndPort(t *testing.T) {
	app, _ := voiceDoctorApp(t)
	no := false
	app.Remote.AllowedUsers[1].Voice = &no
	r := checkVoice(app)
	if !r.ok || !strings.Contains(r.detail, "127.0.0.1:8448") || !strings.Contains(r.detail, `broker tool "whisper"`) ||
		!strings.Contains(r.detail, "no dictation for c@x") || !strings.Contains(r.detail, "never stored") {
		t.Fatalf("%+v", r)
	}
	// A config that bypassed validation: the row still fails.
	app.Manager.AllowPorts = []int{8448}
	if r := checkVoice(app); r.ok || !strings.Contains(r.detail, "jail may reach") {
		t.Fatalf("%+v", r)
	}
	app.Manager.AllowPorts = nil
	app.Broker.Tools = nil
	if r := checkVoice(app); !r.ok || !strings.Contains(r.detail, "yours to run") {
		t.Fatalf("no tool: %+v", r)
	}
}

type readyStub struct{ ready atomic.Bool }

func (s *readyStub) Ready() bool { return s.ready.Load() }
func (s *readyStub) Transcribe(context.Context, []byte) (string, error) {
	return "", voice.ErrNotSent
}

func TestCheckVoiceTool(t *testing.T) {
	app, _ := voiceDoctorApp(t)
	if r := checkVoiceTool(context.Background(), app); r.ok || !strings.Contains(r.detail, "not running") || !strings.Contains(r.fix, "tool-logs/whisper.log") {
		t.Fatalf("no socket: %+v", r)
	}
	ln, err := voice.ListenDictate(app.Remote.Voice.Socket)
	if err != nil {
		t.Fatal(err)
	}
	stub := &readyStub{}
	srv := voice.NewDictateServer(&voice.DictateHandler{Svc: stub, Queue: &voice.Queue{}, Model: "large-v3-turbo", MaxSeconds: 300})
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	if r := checkVoiceTool(context.Background(), app); r.ok || !strings.Contains(r.detail, "not ready") {
		t.Fatalf("not ready: %+v", r)
	}
	stub.ready.Store(true)
	if r := checkVoiceTool(context.Background(), app); !r.ok || !strings.Contains(r.detail, "ready: model large-v3-turbo, at most 300 s") {
		t.Fatalf("ready: %+v", r)
	}
}

func TestCheckVoiceModel(t *testing.T) {
	app, _ := voiceDoctorApp(t)
	w, _ := app.VoiceTool()
	// The real table, while its values are UNVERIFIED: refused.
	if m, _ := voice.Lookup(voice.DefaultModel); m.Pinned() != "" {
		if r := checkVoiceModel(app); r.ok || !strings.Contains(r.detail, "not verified") {
			t.Fatalf("unverified table: %+v", r)
		}
	}
	m := pinTestModel(t, "weights")
	if r := checkVoiceModel(app); r.ok || !strings.Contains(r.fix, "lever voice fetch "+m.Name) {
		t.Fatalf("missing: %+v", r)
	}
	if err := os.MkdirAll(w.Models, 0o700); err != nil {
		t.Fatal(err)
	}
	p := voice.ModelPath(w.Models, m)
	if err := os.WriteFile(p, []byte("weightz"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := checkVoiceModel(app); r.ok || !strings.Contains(r.detail, "sha256") {
		t.Fatalf("wrong file: %+v", r)
	}
	if err := os.WriteFile(p, []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := checkVoiceModel(app); !r.ok || !strings.Contains(r.detail, "sha256 matches") {
		t.Fatalf("good: %+v", r)
	}
	app.Broker.Tools[0].Command = append(app.Broker.Tools[0].Command, "-model", "medium")
	if r := checkVoiceModel(app); r.ok || !strings.Contains(r.detail, "not in lever's model table") {
		t.Fatalf("unknown model: %+v", r)
	}
	app.Broker.Tools = nil
	if r := checkVoiceModel(app); !r.ok || !strings.Contains(r.detail, "not checked") {
		t.Fatalf("no tool: %+v", r)
	}
}

func TestRunVoiceFetch(t *testing.T) {
	app, st := voiceDoctorApp(t)
	w, _ := app.VoiceTool()
	m := pinTestModel(t, "weights")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("weights")) }))
	defer srv.Close()
	fetch := func(ctx context.Context, m voice.Model, dir string) (string, error) {
		f := voice.Fetcher{AllowHost: func(u *url.URL) bool { return "http://"+u.Host == srv.URL }}
		return f.FetchFrom(ctx, m, srv.URL+"/x", dir)
	}
	var out bytes.Buffer
	// Into the tool's -models.
	if err := runVoiceFetch(context.Background(), app, st, "", fetch, &out); err != nil {
		t.Fatal(err)
	}
	if err := voice.Verify(voice.ModelPath(w.Models, m), m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "verified") || !strings.Contains(out.String(), "picks it up") {
		t.Fatalf("%s", out.String())
	}
	if r := checkVoiceModel(app); !r.ok {
		t.Fatalf("doctor after the fetch: %+v", r)
	}
	if err := runVoiceFetch(context.Background(), app, st, "medium", fetch, &out); err == nil || !strings.Contains(err.Error(), "not in lever's pinned model table") {
		t.Fatalf("unknown model: %v", err)
	}
	// No tool configured: the state directory, which must be outside the tree.
	app.Broker.Tools = nil
	out.Reset()
	if err := runVoiceFetch(context.Background(), app, st, "", fetch, &out); err != nil {
		t.Fatal(err)
	}
	if err := voice.Verify(voice.ModelPath(st.VoiceModels(), m), m); err != nil || !strings.Contains(out.String(), "-models "+st.VoiceModels()) {
		t.Fatalf("%v %s", err, out.String())
	}
	inApp, inSt := filesApp(t, true)
	if err := runVoiceFetch(context.Background(), inApp, inSt, "", fetch, &out); err == nil || !strings.Contains(err.Error(), "inside the tree") {
		t.Fatalf("state in tree: %v", err)
	}
}

func TestRunVoiceFetchRefusesUnverified(t *testing.T) {
	app, st := voiceDoctorApp(t)
	m := pinTestModel(t, "w")
	m.SHA256 = voice.Unverified
	voice.Models = []voice.Model{m}
	called := false
	fetch := func(context.Context, voice.Model, string) (string, error) { called = true; return "", nil }
	err := runVoiceFetch(context.Background(), app, st, "", fetch, &bytes.Buffer{})
	if err == nil || called || !strings.Contains(err.Error(), "not verified") {
		t.Fatalf("%v %v", err, called)
	}
}

func TestRemoteVoiceConfig(t *testing.T) {
	app, _ := voiceDoctorApp(t)
	vc := remoteVoiceConfig(app)
	if vc == nil || vc.MaxSeconds != 300 || !vc.ReadAloud || vc.Available() {
		t.Fatalf("%+v (not available without the tool)", vc)
	}
	if !vc.NotSent(voice.ErrNotSent) {
		t.Fatal("NotSent")
	}
	app.Remote.Voice.Enabled = false
	if remoteVoiceConfig(app) != nil {
		t.Fatal("off")
	}
}
