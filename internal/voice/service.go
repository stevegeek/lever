package voice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotReady: whisper-server is not running (starting, restarting, or held
// back because the program or the model failed its check).
var ErrNotReady = errors.New("whisper-server is not ready")

// Service is whisper-server as lever-tool-whisper runs it: the program and
// the model checked before every start, then the supervised child on host
// loopback. The tool asks Ready (its /health) and Transcribe.
type Service struct {
	Model    Model
	ModelDir string // -models: the model's directory, and the child's working directory
	Server   string // -server: the whisper-server program
	Port     int    // -whisper-port
	GPU      bool
	Prompt   string // the vocabulary, as Whisper's prompt
	Language string // "" = auto-detect
	// Log receives lever's own lines; nil drops them.
	Log func(format string, a ...any)
	// Supervisor overrides the child's timings (tests); its Program, Args,
	// Port, Dir and Check are set by Run.
	Supervisor *Supervisor
	// MaxWait bounds one transcription; zero = 10 minutes.
	MaxWait time.Duration

	sup    atomic.Pointer[Supervisor]
	client http.Client
	// path is the child's secret route prefix, fresh per Run (never logged).
	path string

	mu   sync.Mutex
	real string // the program's real path, pinned at its first good check
}

// Run supervises the child until ctx ends. Before every start, the program
// (CheckServer) and the model (pinned table entry, size, sha256) are
// checked again; a failed check logs why and is retried after the backoff,
// so a model fetched later is picked up without a restart. Nothing is
// downloaded here, ever: that is `lever voice fetch`.
func (s *Service) Run(ctx context.Context) error {
	sup := &Supervisor{}
	if s.Supervisor != nil {
		sup = s.Supervisor
	}
	path, err := NewRequestPath()
	if err != nil {
		return fmt.Errorf("voice: no random route prefix: %w", err)
	}
	s.path = path
	p := ModelPath(s.ModelDir, s.Model)
	sup.Args, sup.Port = ServerArgs(p, s.Port, s.GPU, path), s.Port
	// The model directory: outside the tree (config load and the tool's
	// flags refuse it inside), so the server's relative static-file path
	// resolves nowhere an agent writes (whisper.go, assumption 7).
	sup.Dir = s.ModelDir
	if sup.Log == nil {
		sup.Log = s.Log
	}
	sup.Check = s.check
	// Never through a proxy from the environment: loopback only. A fresh
	// connection per clip: a pooled one could be dead after the child
	// restarted, and a clip sent on it would fail as sent (no refund).
	s.client = http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	s.sup.Store(sup)
	sup.Run(ctx)
	return nil
}

// check runs before every start: the program must pass CheckServer and
// still resolve to the real file it resolved to the first time, and the
// model must verify (the child loads it again on every start).
func (s *Service) check() error {
	if why := s.Model.Pinned(); why != "" {
		return fmt.Errorf("%w: %s", ErrUnpinned, why)
	}
	real, err := CheckServer(s.Server)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.real == "" {
		s.real = real
	}
	pinned := s.real
	s.mu.Unlock()
	if real != pinned {
		return fmt.Errorf("voice: -server now resolves to %s, not %s; restart the tool (lever reload) to accept it", real, pinned)
	}
	if err := Verify(ModelPath(s.ModelDir, s.Model), s.Model); err != nil {
		return fmt.Errorf("the model %s does not verify: %w (run `lever voice fetch %s`)", s.Model.Name, err, s.Model.Name)
	}
	if sup := s.sup.Load(); sup != nil {
		sup.Program = real
	}
	return nil
}

// Ready reports whether the child is running and answers on its port.
func (s *Service) Ready() bool {
	sup := s.sup.Load()
	return sup != nil && sup.Ready()
}

// Transcribe sends one checked WAV clip to the child and returns the
// transcript as whisper-server gave it.
func (s *Service) Transcribe(ctx context.Context, wav []byte) (string, error) {
	sup := s.sup.Load()
	if sup == nil || !sup.Ready() {
		return "", fmt.Errorf("%w: %w", ErrNotSent, ErrNotReady)
	}
	wait := s.MaxWait
	if wait <= 0 {
		wait = 10 * time.Minute
	}
	// Bound to the caller: when the caller gives up, the request to the
	// child is closed, and whisper-server stops work on a clip whose
	// connection closed (its abort callback, whisper.go assumption 9), so
	// the GPU frees with the work.
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	return Transcribe(ctx, &s.client, sup.Addr(), Request{Path: s.path, WAV: wav, Prompt: s.Prompt, Language: s.Language})
}
