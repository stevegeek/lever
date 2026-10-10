// Command lever-tool-whisper is the host-side broker tool for speech to text.
// It runs whisper.cpp's whisper-server as its child on host loopback
// (internal/voice: the supervisor, the pinned model check, the adapter) and
// offers it two ways:
//
//   - dictation for the chat page: the remote proxy posts a checked clip to
//     the tool's Unix socket (-dictate-socket, remote.voice.socket), which
//     only the operator's user reaches; never through the broker gateway;
//   - the MCP operation transcribe for agents, through the broker like any
//     first-party tool, with a capability from an obtain grant {tool:
//     whisper, op: transcribe}: a WAV file in the tree root's
//     .lever-files/whisper/ (/workspace/.lever-files/whisper/ in the
//     manager's container, which only the manager can write), whoever the
//     caller is. Without a capability, no agent can use it.
//
// One clip at a time reaches whisper-server; a waiting dictation clip goes
// first in the queue, but a clip already running is never interrupted, so
// an agent clip (at most -agent-max-seconds long) can keep dictation waiting
// for one transcription. Audio and transcripts are never stored or logged.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/stevegeek/lever/captool"
	"github.com/stevegeek/lever/internal/voice"
)

// Version is stamped by the Makefile (-X main.Version=...).
var Version = "dev"

type opts struct {
	name, backend, admin, tree, models, server, socket, language string
	model                                                        voice.Model
	prompt                                                       string
	gpu                                                          bool
	whisperPort, maxSeconds, agentMaxSeconds                     int
}

func parseFlags(args []string) (opts, error) {
	var o opts
	var model, vocabulary string
	fs := flag.NewFlagSet("lever-tool-whisper", flag.ContinueOnError)
	fs.StringVar(&o.name, "name", "whisper", "tool/registry name")
	fs.StringVar(&o.backend, "backend", "127.0.0.1:3212", "MCP listen address (set by the broker)")
	fs.StringVar(&o.admin, "admin", "", "broker admin base URL (set by the broker)")
	fs.StringVar(&o.tree, "tree", "", "absolute instance tree path")
	fs.StringVar(&o.models, "models", "", "absolute directory holding the models `lever voice fetch` downloads (outside -tree)")
	fs.StringVar(&o.server, "server", "", "absolute path of whisper.cpp's whisper-server (outside -tree)")
	fs.StringVar(&model, "model", voice.DefaultModel, "model name from lever's pinned table: "+strings.Join(voice.Names(), ", "))
	fs.StringVar(&o.language, "language", "", "Whisper language code (empty = detect per clip)")
	fs.StringVar(&vocabulary, "vocabulary", "", "comma list of words Whisper should expect")
	fs.BoolVar(&o.gpu, "gpu", true, "use the GPU (-gpu=false passes --no-gpu)")
	fs.IntVar(&o.whisperPort, "whisper-port", 0, "host loopback port for the whisper-server child")
	fs.StringVar(&o.socket, "dictate-socket", "", "absolute path of the dictation Unix socket (outside -tree)")
	fs.IntVar(&o.maxSeconds, "max-seconds", voice.DefaultMaxSeconds, "longest clip, in seconds")
	fs.IntVar(&o.agentMaxSeconds, "agent-max-seconds", 0, fmt.Sprintf("longest clip of the agent operation transcribe, in seconds, at most -max-seconds (default %d, or -max-seconds if smaller)", voice.DefaultAgentMaxSeconds))
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q (a bool flag takes its value as -gpu=false)", fs.Arg(0))
	}
	for _, r := range []struct{ flag, v string }{{"-tree", o.tree}, {"-models", o.models}, {"-server", o.server}, {"-dictate-socket", o.socket}} {
		if r.v == "" {
			return o, fmt.Errorf("%s is required", r.flag)
		}
		if !filepath.IsAbs(r.v) {
			return o, fmt.Errorf("%s must be an absolute path, got %q", r.flag, r.v)
		}
	}
	if err := voice.CheckSocketPath(o.socket); err != nil {
		return o, fmt.Errorf("-dictate-socket: %w", err)
	}
	for _, r := range []struct{ flag, v, why string }{
		{"-models", o.models, "an agent could replace the model whisper-server loads"},
		{"-server", o.server, "an agent could replace the program the host runs"},
		{"-dictate-socket", o.socket, "an agent could reach or replace the dictation socket"},
	} {
		if err := refuseInTree(o.tree, r.flag, r.v, r.why); err != nil {
			return o, err
		}
	}
	m, ok := voice.Lookup(model)
	if !ok {
		return o, fmt.Errorf("-model %q is not in lever's pinned model table; use one of %s", model, strings.Join(voice.Names(), ", "))
	}
	if why := m.Pinned(); why != "" {
		return o, fmt.Errorf("-model: %s", why)
	}
	o.model = m
	if err := voice.CheckLanguage(o.language); err != nil {
		return o, fmt.Errorf("-language: %w", err)
	}
	p, err := voice.ParseVocabulary(vocabulary)
	if err != nil {
		return o, fmt.Errorf("-vocabulary: %w", err)
	}
	o.prompt = p
	if o.whisperPort < 1 || o.whisperPort > 65535 {
		return o, fmt.Errorf("-whisper-port is required: a free host loopback port (1-65535) the jail cannot reach")
	}
	if _, port, err := net.SplitHostPort(o.backend); err == nil && port == strconv.Itoa(o.whisperPort) {
		return o, fmt.Errorf("-whisper-port %d is the tool's own -backend port", o.whisperPort)
	}
	if err := voice.CheckMaxSeconds(o.maxSeconds); err != nil {
		return o, fmt.Errorf("-max-seconds: %w", err)
	}
	agentSet := false
	fs.Visit(func(f *flag.Flag) { agentSet = agentSet || f.Name == "agent-max-seconds" })
	if agentSet {
		if err := voice.CheckAgentMaxSeconds(o.agentMaxSeconds, o.maxSeconds); err != nil {
			return o, fmt.Errorf("-agent-max-seconds: %w", err)
		}
	}
	o.agentMaxSeconds = voice.AgentMaxSeconds(o.agentMaxSeconds, o.maxSeconds)
	return o, nil
}

// transcribeBackstop re-checks the file name after token verification,
// whatever the token's caveats say.
func transcribeBackstop(c captool.ValidatedContext, a map[string]string) error {
	if c.Operation != "transcribe" {
		return fmt.Errorf("whisper: backstop: only transcribe is permitted")
	}
	return voice.CheckAgentFile(a["file"])
}

// asResult turns a refusal into a result the agent can read (captool maps a
// Handler error to a bare "tool error"). The messages name no host path and
// never carry a transcript.
func asResult(v any, err error) (any, error) {
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}, nil
	}
	return v, nil
}

func main() {
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	// The program is checked once here (a missing or unsafe program is a
	// config error), and again before every start of the child.
	if _, err := voice.CheckServer(o.server); err != nil {
		log.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	vlog := func(format string, a ...any) { log.Printf(format, a...) }
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	ln, err := voice.ListenDictate(o.socket)
	if err != nil {
		log.Fatal(err)
	}
	svc := &voice.Service{Model: o.model, ModelDir: o.models, Server: o.server, Port: o.whisperPort,
		GPU: o.gpu, Prompt: o.prompt, Language: o.language, Log: vlog}
	queue := &voice.Queue{}
	agents := &voice.AgentTranscriber{Tree: o.tree, MaxSeconds: o.agentMaxSeconds, MaxWait: voice.AgentMaxWait(o.agentMaxSeconds, o.gpu),
		Svc: svc, Queue: queue, Log: vlog}
	srv, err := captool.New(captool.Config{
		Name: o.name, Version: Version, Backend: o.backend, AdminURL: o.admin, Log: logger,
		Operations: []captool.Operation{{
			Name: "transcribe",
			Description: fmt.Sprintf("transcribe one WAV file from /workspace/%s/ (PCM s16le, 16 kHz, mono, canonical 44-byte header, at most %d s); returns {\"text\": ...}",
				voice.AgentDir, o.agentMaxSeconds),
			Params: []captool.ParamSpec{
				{Name: "file", Type: "string", Description: "file name in /workspace/" + voice.AgentDir + "/, e.g. note-1.wav"},
			},
			Backstop: transcribeBackstop,
			Handler: func(c captool.ValidatedContext, a map[string]string) (any, error) {
				text, err := agents.Transcribe(ctx, c.Caller, a["file"])
				return asResult(map[string]any{"text": text}, err)
			},
		}},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := srv.Register(ctx); err != nil {
		log.Fatalf("register with broker: %v", err)
	}

	svcDone := make(chan struct{})
	go func() {
		defer close(svcDone)
		if err := svc.Run(ctx); err != nil {
			log.Printf("voice: %v", err)
		}
	}()
	dictate := voice.NewDictateServer(&voice.DictateHandler{Svc: svc, Queue: queue, Model: o.model.Name, MaxSeconds: o.maxSeconds, Log: vlog})
	go func() {
		if err := dictate.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("voice: dictation socket: %v", err)
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
	log.Printf("lever-tool-whisper %q serving MCP on %s, dictation on %s; model %s, whisper-server on 127.0.0.1:%d (host loopback only)",
		o.name, o.backend, o.socket, o.model.Name, o.whisperPort)
	<-ctx.Done()
	// Stop the child first (the supervisor waits for it), then the servers;
	// closing the socket's listener removes the socket file.
	<-svcDone
	_ = dictate.Close()
	_ = mcp.Close()
}

// resolveExisting resolves symlinks of the nearest existing ancestor of p and
// re-appends the part that does not exist yet.
func resolveExisting(p string) (string, error) {
	p = filepath.Clean(p)
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", fmt.Errorf("cannot resolve %q", p)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// refuseInTree: the models, the program and the socket must not sit in the
// tree the agent can read and write (or be the tree itself).
func refuseInTree(tree, flag, p, why string) error {
	t, err := filepath.EvalSymlinks(tree)
	if err != nil {
		return fmt.Errorf("-tree: %w", err)
	}
	s, err := resolveExisting(p)
	if err != nil {
		return fmt.Errorf("%s: %w", flag, err)
	}
	rel, err := filepath.Rel(t, s)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s %s is inside -tree %s: %s", flag, p, tree, why)
	}
	return nil
}
