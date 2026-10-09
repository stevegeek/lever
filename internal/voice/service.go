package voice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// ErrNotReady: dictation is configured, but whisper-server is not running
// (starting, restarting, or refused at start).
var ErrNotReady = errors.New("whisper-server is not ready")

// Service is remote.voice as the proxy runs it: the model and program
// checked once at start, then the supervised child. The proxy asks Usable
// (whether the page shows the mic) and Transcribe.
type Service struct {
	Model    Model
	ModelDir string
	Server   string // remote.voice.whisper_server
	Port     int
	GPU      bool
	Prompt   string // the vocabulary, as Whisper's prompt
	Language string // "" = auto-detect
	// Log receives lever's own lines; nil drops them.
	Log func(format string, a ...any)
	// Supervisor overrides the child's timings (tests); its Program, Args
	// and Port are set by Run.
	Supervisor *Supervisor
	// MaxWait bounds one transcription; zero = 10 minutes.
	MaxWait time.Duration

	usable atomic.Bool
	sup    atomic.Pointer[Supervisor]
	client http.Client
}

// Run checks the model (pinned table entry, size, sha256) and the program,
// then supervises the child until ctx ends. A failed check logs why and
// leaves dictation off for this run (the page shows no mic); nothing is
// downloaded here, ever: that is `lever voice fetch`.
func (s *Service) Run(ctx context.Context) {
	p := ModelPath(s.ModelDir, s.Model)
	if err := Verify(p, s.Model); err != nil {
		s.logf("voice: dictation stays off: the model %s does not verify: %v (run `lever voice fetch %s`, then restart the proxy)", s.Model.Name, err, s.Model.Name)
		return
	}
	real, err := CheckServer(s.Server)
	if err != nil {
		s.logf("voice: dictation stays off: %v", err)
		return
	}
	sup := &Supervisor{}
	if s.Supervisor != nil {
		sup = s.Supervisor
	}
	sup.Program, sup.Args, sup.Port = real, ServerArgs(p, s.Port, s.GPU), s.Port
	// The model directory: in the state directory, host-only, so the
	// server's relative static-file path resolves nowhere an agent writes.
	sup.Dir = s.ModelDir
	if sup.Log == nil {
		sup.Log = s.Log
	}
	// Before every restart too: the program must still be the one checked,
	// and must still resolve to the same real file.
	sup.Check = func() error {
		again, err := CheckServer(s.Server)
		if err == nil && again != real {
			err = fmt.Errorf("voice: whisper_server now resolves to %s, not %s; restart the proxy to accept it", again, real)
		}
		return err
	}
	// Never through a proxy from the environment: loopback only.
	s.client = http.Client{Transport: &http.Transport{MaxIdleConns: 1}}
	s.sup.Store(sup)
	s.usable.Store(true)
	defer s.usable.Store(false)
	sup.Run(ctx)
}

func (s *Service) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log(format, a...)
	}
}

// Usable reports whether the checks at start passed and the proxy is
// supervising the child, and the last start was not blocked (the port taken
// by another process, or the program failing its check): the page then
// shows the mic. A child that is restarting still counts; a request then
// answers unavailable.
func (s *Service) Usable() bool {
	sup := s.sup.Load()
	return s.usable.Load() && sup != nil && !sup.Blocked()
}

// Transcribe sends one checked WAV clip to the child and returns the
// transcript.
func (s *Service) Transcribe(ctx context.Context, wav []byte) (string, error) {
	sup := s.sup.Load()
	if !s.Usable() || sup == nil || !sup.Ready() {
		return "", ErrNotReady
	}
	wait := s.MaxWait
	if wait <= 0 {
		wait = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	return Transcribe(ctx, &s.client, sup.Addr(), Request{WAV: wav, Prompt: s.Prompt, Language: s.Language})
}
