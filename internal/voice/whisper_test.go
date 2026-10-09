package voice

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestServerArgs(t *testing.T) {
	got := ServerArgs("/s/ggml.bin", 8448, true)
	if want := []string{"--host", "127.0.0.1", "--port", "8448", "-m", "/s/ggml.bin"}; !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
	if got := ServerArgs("/m", 1, false); got[len(got)-1] != "--no-gpu" {
		t.Fatalf("%q", got)
	}
	for _, a := range ServerArgs("/m", 1, false) {
		if strings.Contains(a, "convert") {
			t.Fatal("lever must never ask whisper-server to convert (ffmpeg)")
		}
	}
}

func TestTranscribeForm(t *testing.T) {
	var form map[string]string
	var file []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/inference" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		form = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			form[k] = v[0]
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		file, _ = io.ReadAll(f)
		_, _ = w.Write([]byte(`{"text":" hello world \n"}`))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	text, err := Transcribe(context.Background(), srv.Client(), addr, Request{WAV: []byte("RIFFdata"), Prompt: "Lever, Scion", Language: "en"})
	if err != nil || text != " hello world \n" {
		t.Fatalf("%q %v", text, err)
	}
	if string(file) != "RIFFdata" || form["response_format"] != "json" || form["prompt"] != "Lever, Scion" || form["language"] != "en" {
		t.Fatalf("%v %q", form, file)
	}
	if _, err := Transcribe(context.Background(), srv.Client(), addr, Request{WAV: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if _, ok := form["prompt"]; ok || form["language"] != "auto" {
		t.Fatalf("no vocabulary, no language: %v", form)
	}
}

func TestTranscribeFailures(t *testing.T) {
	for name, answer := range map[string]func(w http.ResponseWriter){
		"status":   func(w http.ResponseWriter) { http.Error(w, "secret words", 500) },
		"not json": func(w http.ResponseWriter) { _, _ = w.Write([]byte("secret words")) },
		"error":    func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"error":"secret words"}`)) },
		"no text":  func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{}`)) },
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { answer(w) }))
		_, err := Transcribe(context.Background(), srv.Client(), strings.TrimPrefix(srv.URL, "http://"), Request{WAV: []byte("x")})
		srv.Close()
		if !errors.Is(err, ErrServer) {
			t.Errorf("%s: %v", name, err)
		}
		// The server's own words never reach an error (they could quote the
		// audio, and errors go to the audit log).
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
