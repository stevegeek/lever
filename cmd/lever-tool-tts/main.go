// Command lever-tool-tts is the host-side broker tool for text to speech:
// natural read-aloud on the chat page, with a local neural voice (Kokoro, or
// a Piper voice) run by sherpa-onnx on the operator's own host. Agent text
// never goes to a cloud speech service.
//
// The remote proxy posts each part of a message to the tool's Unix socket
// (-speak-socket, remote.voice.speak_socket), which only the operator's user
// reaches; never through the broker gateway. For each part the tool runs
// sherpa-onnx's sherpa-onnx-offline-tts once as its child (internal/voice:
// sherpa.go, speaker.go), with the program and the pinned model checked
// before every run, and answers the WAV. There is no long-running engine and
// no TCP port: nothing listens but the socket and the tool's MCP backend.
//
// The MCP operation voices (read-only: the model and its voices) is what
// the broker registers; it gives an agent nothing to speak with. Text and
// audio are never stored or logged.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/stevegeek/lever/captool"
	"github.com/stevegeek/lever/internal/logging"
	"github.com/stevegeek/lever/internal/voice"
)

// Version is stamped by the Makefile (-X main.Version=...).
var Version = "dev"

type opts struct {
	name, backend, admin, tree, models, server, socket, voice string
	model                                                     voice.SpeechModel
	speed                                                     float64
	threads, maxChars                                         int
}

// DefaultThreads is -threads when unset.
const DefaultThreads = 2

func parseFlags(args []string) (opts, error) {
	var o opts
	var model string
	fs := flag.NewFlagSet("lever-tool-tts", flag.ContinueOnError)
	fs.StringVar(&o.name, "name", "tts", "tool/registry name")
	fs.StringVar(&o.backend, "backend", "127.0.0.1:3213", "MCP listen address (set by the broker)")
	fs.StringVar(&o.admin, "admin", "", "broker admin base URL (set by the broker)")
	fs.StringVar(&o.tree, "tree", "", "absolute instance tree path")
	fs.StringVar(&o.models, "models", "", "absolute directory holding the models lever voice fetch downloads (outside -tree)")
	fs.StringVar(&o.server, "server", "", "absolute path of sherpa-onnx's sherpa-onnx-offline-tts (outside -tree)")
	fs.StringVar(&model, "model", voice.DefaultSpeechModel, "speech model from lever's pinned table: "+strings.Join(voice.SpeechNames(), ", "))
	fs.StringVar(&o.voice, "voice", "", "default voice of the model (empty = the model's default)")
	fs.Float64Var(&o.speed, "speed", 1, fmt.Sprintf("speaking rate, %.1f to %.1f (1 = the model's own)", voice.MinSpeed, voice.MaxSpeed))
	fs.IntVar(&o.threads, "threads", DefaultThreads, fmt.Sprintf("CPU threads of one run, 1 to %d", voice.MaxThreads))
	fs.IntVar(&o.maxChars, "max-chars", voice.DefaultSpeakMaxChars, fmt.Sprintf("longest text of one request, in characters, %d to %d", voice.MinSpeakMaxChars, voice.MaxSpeakMaxChars))
	fs.StringVar(&o.socket, "speak-socket", "", "absolute path of the speech Unix socket (outside -tree)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	for _, r := range []struct{ flag, v string }{{"-tree", o.tree}, {"-models", o.models}, {"-server", o.server}, {"-speak-socket", o.socket}} {
		if r.v == "" {
			return o, fmt.Errorf("%s is required", r.flag)
		}
		if !filepath.IsAbs(r.v) {
			return o, fmt.Errorf("%s must be an absolute path, got %q", r.flag, r.v)
		}
	}
	if err := voice.CheckSocketPath(o.socket); err != nil {
		return o, fmt.Errorf("-speak-socket: %w", err)
	}
	for _, r := range []struct{ flag, v, why string }{
		{"-models", o.models, "an agent could replace the model the engine loads"},
		{"-server", o.server, "an agent could replace the program the host runs"},
		{"-speak-socket", o.socket, "an agent could reach or replace the speech socket"},
	} {
		if err := voice.RefuseInTree(o.tree, r.flag, r.v, r.why); err != nil {
			return o, err
		}
	}
	m, ok := voice.LookupSpeech(model)
	if !ok {
		return o, fmt.Errorf("-model %q is not in lever's pinned speech model table; use one of %s", model, strings.Join(voice.SpeechNames(), ", "))
	}
	if why := m.Pinned(); why != "" {
		return o, fmt.Errorf("-model: %s", why)
	}
	o.model = m
	if o.voice == "" {
		o.voice = m.DefaultVoice
	}
	if _, ok := m.Voice(o.voice); !ok {
		return o, fmt.Errorf("-voice %q is not a voice of %s; use one of %s", o.voice, m.Name, strings.Join(m.VoiceNames(), ", "))
	}
	if err := voice.CheckSpeed(o.speed); err != nil {
		return o, fmt.Errorf("-speed: %w", err)
	}
	if err := voice.CheckThreads(o.threads); err != nil {
		return o, fmt.Errorf("-threads: %w", err)
	}
	if err := voice.CheckSpeakMaxChars(o.maxChars); err != nil {
		return o, fmt.Errorf("-max-chars: %w", err)
	}
	return o, nil
}

// voicesBackstop re-checks the operation after token verification.
func voicesBackstop(c captool.ValidatedContext, _ map[string]string) error {
	if c.Operation != "voices" {
		return fmt.Errorf("tts: backstop: only voices is permitted")
	}
	return nil
}

func main() {
	logger := logging.Install(os.Stderr, "lever-tool-tts")
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	// The program is checked once here (a missing or unsafe program is a
	// config error), and again before every run.
	if _, err := voice.CheckServer(o.server); err != nil {
		log.Fatal(err)
	}
	if err := voice.PrivateDir(o.models); err != nil {
		log.Fatalf("-models: %v", err)
	}
	voice.RemoveStaleSpeech(o.models, time.Now())
	vlog := func(format string, a ...any) { log.Printf(format, a...) }
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	ln, err := voice.ListenSpeak(o.socket)
	if err != nil {
		log.Fatal(err)
	}
	spk := &voice.Speaker{Model: o.model, ModelDir: o.models, Server: o.server, Voice: o.voice, Speed: o.speed,
		Threads: o.threads, MaxChars: o.maxChars, Log: vlog}
	if _, err := spk.Check(); err != nil {
		// Not fatal: a model fetched later is picked up at the next request.
		log.Printf("voice: speech not ready yet: %v", err)
	}
	handler := &voice.SpeakHandler{Engine: spk, Model: o.model, Voice: o.voice, MaxChars: o.maxChars, Log: vlog}
	srv, err := captool.New(captool.Config{
		Name: o.name, Version: Version, Backend: o.backend, AdminURL: o.admin, Log: logger,
		Operations: []captool.Operation{{
			Name:        "voices",
			Description: "list the read-aloud voices of the operator's host: {\"ready\": bool, \"model\": ..., \"voice\": the default, \"voices\": [{\"name\", \"lang\"}]}",
			Backstop:    voicesBackstop,
			Handler: func(captool.ValidatedContext, map[string]string) (any, error) {
				h := handler.Health()
				return map[string]any{"ready": h.Ready, "model": h.Model, "voice": h.Voice, "voices": h.Voices}, nil
			},
		}},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := srv.Register(ctx); err != nil {
		log.Fatalf("register with broker: %v", err)
	}
	speak := voice.NewSpeakServer(handler)
	go func() {
		if err := speak.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("voice: speech socket: %v", err)
			stop()
		}
	}()
	mcp := &http.Server{Addr: o.backend, Handler: srv.Handler()}
	go func() {
		if err := mcp.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("serve MCP: %v", err)
			stop()
		}
	}()
	log.Printf("lever-tool-tts %q serving MCP on %s, speech on %s; model %s, voice %s, one sherpa-onnx run per request (no port)",
		o.name, o.backend, o.socket, o.model.Name, o.voice)
	<-ctx.Done()
	// Closing the listener removes the socket file. A run in progress is
	// killed with the request it serves.
	_ = speak.Close()
	_ = mcp.Close()
}
