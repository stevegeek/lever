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

// voiceDoctorApp is filesApp with voice on, and an executable
// whisper-server outside the tree.
func voiceDoctorApp(t *testing.T) (*config.App, state.State) {
	t.Helper()
	app, st := filesApp(t, false)
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	prog := filepath.Join(dir, "whisper-server")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	app.Remote.Voice = config.Voice{Enabled: true, WhisperServer: prog}
	return app, st
}

func TestCheckVoiceOff(t *testing.T) {
	app, st := filesApp(t, false)
	if r := checkVoice(app); !r.ok || !strings.HasPrefix(r.detail, "off") {
		t.Fatalf("%+v", r)
	}
	if r := checkVoiceModel(app, st); r.name != "" {
		t.Fatalf("off: a model row %+v", r)
	}
	if r := checkVoiceServer(app); r.name != "" {
		t.Fatalf("off: a server row %+v", r)
	}
}

func TestCheckVoiceConfigAndPort(t *testing.T) {
	app, _ := voiceDoctorApp(t)
	no := false
	app.Remote.AllowedUsers[1].Voice = &no
	r := checkVoice(app)
	if !r.ok || !strings.Contains(r.detail, "127.0.0.1:8448") || !strings.Contains(r.detail, "no dictation for c@x") || !strings.Contains(r.detail, "never stored") {
		t.Fatalf("%+v", r)
	}
	// A config that bypassed validation: the row still fails.
	app.Manager.AllowPorts = []int{config.DefaultRemoteVoicePort}
	if r := checkVoice(app); r.ok || !strings.Contains(r.detail, "jail may reach") {
		t.Fatalf("%+v", r)
	}
}

func TestCheckVoiceModel(t *testing.T) {
	app, st := voiceDoctorApp(t)
	// The real table, while its values are UNVERIFIED: refused.
	if m, _ := voice.Lookup(voice.DefaultModel); m.Pinned() != "" {
		if r := checkVoiceModel(app, st); r.ok || !strings.Contains(r.detail, "not verified") {
			t.Fatalf("unverified table: %+v", r)
		}
	}
	m := pinTestModel(t, "weights")
	if r := checkVoiceModel(app, st); r.ok || !strings.Contains(r.fix, "lever voice fetch "+m.Name) {
		t.Fatalf("missing: %+v", r)
	}
	if err := os.MkdirAll(st.VoiceModels(), 0o700); err != nil {
		t.Fatal(err)
	}
	p := voice.ModelPath(st.VoiceModels(), m)
	if err := os.WriteFile(p, []byte("weightz"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := checkVoiceModel(app, st); r.ok || !strings.Contains(r.detail, "sha256") {
		t.Fatalf("wrong file: %+v", r)
	}
	if err := os.WriteFile(p, []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := checkVoiceModel(app, st); !r.ok || !strings.Contains(r.detail, "sha256 matches") {
		t.Fatalf("good: %+v", r)
	}
	inApp, inSt := filesApp(t, true)
	inApp.Remote.Voice = app.Remote.Voice
	if r := checkVoiceModel(inApp, inSt); r.ok || !strings.Contains(r.detail, "inside the tree") {
		t.Fatalf("state in tree: %+v", r)
	}
}

func TestCheckVoiceServer(t *testing.T) {
	app, _ := voiceDoctorApp(t)
	if r := checkVoiceServer(app); !r.ok {
		t.Fatalf("%+v", r)
	}
	if err := os.Chmod(app.Remote.Voice.WhisperServer, 0o766); err != nil {
		t.Fatal(err)
	}
	if r := checkVoiceServer(app); r.ok || !strings.Contains(r.fix, "outside the tree") {
		t.Fatalf("writable by others: %+v", r)
	}
	app.Remote.Voice.WhisperServer = filepath.Join(t.TempDir(), "missing")
	if r := checkVoiceServer(app); r.ok {
		t.Fatalf("missing: %+v", r)
	}
}

func TestRunVoiceFetch(t *testing.T) {
	app, st := voiceDoctorApp(t)
	m := pinTestModel(t, "weights")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("weights")) }))
	defer srv.Close()
	fetch := func(ctx context.Context, m voice.Model, dir string) (string, error) {
		f := voice.Fetcher{AllowHost: func(u *url.URL) bool { return "http://"+u.Host == srv.URL }}
		return f.FetchFrom(ctx, m, srv.URL+"/x", dir)
	}
	var out bytes.Buffer
	if err := runVoiceFetch(context.Background(), app, st, "", fetch, &out); err != nil {
		t.Fatal(err)
	}
	if err := voice.Verify(voice.ModelPath(st.VoiceModels(), m), m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "verified") || !strings.Contains(out.String(), "restart the remote proxy") {
		t.Fatalf("%s", out.String())
	}
	if r := checkVoiceModel(app, st); !r.ok {
		t.Fatalf("doctor after the fetch: %+v", r)
	}
	if err := runVoiceFetch(context.Background(), app, st, "medium", fetch, &out); err == nil || !strings.Contains(err.Error(), "not in lever's pinned model table") {
		t.Fatalf("unknown model: %v", err)
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

func TestRemoteVoiceService(t *testing.T) {
	app, st := voiceDoctorApp(t)
	pinTestModel(t, "weights")
	app.Remote.Voice.Vocabulary = []string{"Lever", "Scion"}
	app.Remote.Voice.Language = "en"
	vs := remoteVoice(app, st, &bytes.Buffer{})
	if vs == nil || vs.Port != config.DefaultRemoteVoicePort || vs.Prompt != "Lever, Scion" || vs.Language != "en" || !vs.GPU ||
		vs.ModelDir != st.VoiceModels() || vs.Server != app.Remote.Voice.WhisperServer {
		t.Fatalf("%+v", vs)
	}
	vc := remoteVoiceConfig(app, vs)
	if vc.MaxSeconds != 300 || vc.Available() {
		t.Fatalf("%+v (not usable before it runs)", vc)
	}
	var warn bytes.Buffer
	inApp, inSt := filesApp(t, true)
	inApp.Remote.Voice = app.Remote.Voice
	if remoteVoice(inApp, inSt, &warn) != nil || !strings.Contains(warn.String(), "dictation stays off") {
		t.Fatalf("state in tree: %q", warn.String())
	}
	app.Remote.Voice.Enabled = false
	if remoteVoice(app, st, &warn) != nil || remoteVoiceConfig(app, nil) != nil {
		t.Fatal("off")
	}
}
