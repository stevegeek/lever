package voice

// The dictation socket: how the remote proxy reaches lever-tool-whisper.
//
// The tool listens on a Unix socket in a private 0700 directory, the socket
// itself 0600: only the operator's processes reach it, and the jail never
// does (it reaches no host Unix socket, and config load refuses a path
// inside the tree). Dictation deliberately avoids the broker gateway and the
// tool's TCP backend: the gateway forwards every path under /mcp/<tool>/, so
// a host-only route there would be one step from agents.
//
// Routes:
//
//	POST /dictate  body: one WAV clip (CheckWAV). 200 {"text": "..."}, or an
//	               error status with {"error": word}, one of the Word*
//	               constants.
//	GET  /health   200 {"ready": bool, "model": name, "maxSeconds": n}.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Error words of the dictation socket, beyond CheckWAV's.
const (
	// WordNotReady: whisper-server is not running; the clip did not reach it.
	WordNotReady = "not-ready"
	// WordFailed: whisper-server had the clip and gave no transcript (or
	// the wait ended while it worked).
	WordFailed = "failed"
	// WordCanceled: the caller went away while the clip waited in the
	// queue. The clip counts, though whisper-server never saw it.
	WordCanceled = "canceled"
	// WordInterrupted: the caller went away while whisper-server worked on
	// the clip.
	WordInterrupted = "interrupted"
)

// MaxSocketPath: sun_path holds 104 bytes on macOS and 108 on Linux, the
// NUL included.
const MaxSocketPath = 103

// ErrTool: lever-tool-whisper answered, but not with a transcript.
var ErrTool = errors.New("lever-tool-whisper")

// CheckSocketPath accepts a clean absolute path short enough for a Unix
// socket. Staying outside the tree is config load's check.
func CheckSocketPath(p string) error {
	switch {
	case !filepath.IsAbs(p) || filepath.Clean(p) != p:
		return fmt.Errorf("socket %q must be a clean absolute path", p)
	case len(p) > MaxSocketPath:
		return fmt.Errorf("socket %q is %d bytes; a Unix socket path holds at most %d", p, len(p), MaxSocketPath)
	}
	return nil
}

// ListenDictate creates the dictation socket (0600) at path. Its directory
// is created 0700 when missing and must be a private directory of the
// caller's (PrivateDir). A stale socket is removed only when it is the
// caller's and nothing answers on it; anything else at path is refused.
func ListenDictate(path string) (net.Listener, error) {
	return listenPrivate(path, "dictation socket", "lever-tool-whisper")
}

func listenPrivate(path, what, tool string) (net.Listener, error) {
	if err := CheckSocketPath(path); err != nil {
		return nil, err
	}
	if err := PrivateDir(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("%s directory: %w", what, err)
	}
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	case fi.Mode()&fs.ModeSocket == 0:
		return nil, fmt.Errorf("%s %s: something that is not a socket is there (%s); lever leaves it alone", what, path, fi.Mode().Type())
	default:
		if owner, ok := fileOwner(fi); ok && owner != os.Getuid() {
			return nil, fmt.Errorf("%s %s belongs to uid %d, not to you", what, path, owner)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s %s is in use: another %s serves it", what, path, tool)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("%s %s: removing the stale socket: %w", what, path, err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

type Transcriber interface {
	Ready() bool
	Transcribe(ctx context.Context, wav []byte) (string, error)
}

type Health struct {
	Ready      bool   `json:"ready"`
	Model      string `json:"model"`
	MaxSeconds int    `json:"maxSeconds"`
}

type DictateHandler struct {
	Svc        Transcriber
	Queue      *Queue
	Model      string
	MaxSeconds int
	// Log receives one line per clip: length, outcome and latency; never the
	// audio, never the text. nil drops them.
	Log func(format string, a ...any)
}

// NewDictateServer sets header and body deadlines but no write deadline: a
// transcription takes what it takes, and Service bounds it.
func NewDictateServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 2 * time.Minute, IdleTimeout: time.Minute}
}

func (h *DictateHandler) logf(format string, a ...any) {
	if h.Log != nil {
		h.Log(format, a...)
	}
}

func answerJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *DictateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/health" && r.Method == http.MethodGet:
		answerJSON(w, http.StatusOK, Health{Ready: h.Svc.Ready(), Model: h.Model, MaxSeconds: h.MaxSeconds})
	case r.URL.Path == "/dictate" && r.Method == http.MethodPost:
		h.dictate(w, r)
	case r.URL.Path == "/health" || r.URL.Path == "/dictate":
		answerJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method"})
	default:
		answerJSON(w, http.StatusNotFound, map[string]string{"error": "not-found"})
	}
}

func (h *DictateHandler) dictate(w http.ResponseWriter, r *http.Request) {
	limit := MaxClipBytes(h.MaxSeconds)
	if r.ContentLength > limit {
		drainClip(r)
		answerJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": WordTooLong})
		return
	}
	clip, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		answerJSON(w, http.StatusBadRequest, map[string]string{"error": WordBadAudio})
		return
	}
	seconds, word := CheckWAV(clip, h.MaxSeconds)
	if word != "" {
		drainClip(r)
		status := http.StatusBadRequest
		if word == WordTooLong {
			status = http.StatusRequestEntityTooLarge
		}
		answerJSON(w, status, map[string]string{"error": word})
		return
	}
	if !h.Svc.Ready() {
		h.logf("voice: dictation clip %.1f s: whisper-server not ready", seconds)
		answerJSON(w, http.StatusServiceUnavailable, map[string]string{"error": WordNotReady})
		return
	}
	if err := h.Queue.Acquire(r.Context(), true); err != nil {
		h.logf("voice: dictation clip %.1f s: canceled while queued", seconds)
		answerJSON(w, http.StatusServiceUnavailable, map[string]string{"error": WordCanceled})
		return
	}
	if r.Context().Err() != nil {
		h.Queue.Release()
		h.logf("voice: dictation clip %.1f s: canceled while queued", seconds)
		answerJSON(w, http.StatusServiceUnavailable, map[string]string{"error": WordCanceled})
		return
	}
	start := time.Now()
	text, err := h.Svc.Transcribe(r.Context(), clip)
	h.Queue.Release()
	ms := time.Since(start).Milliseconds()
	switch {
	case errors.Is(err, ErrNotSent):
		h.logf("voice: dictation clip %.1f s: did not reach whisper-server: %v", seconds, err)
		answerJSON(w, http.StatusServiceUnavailable, map[string]string{"error": WordNotReady})
	case err != nil && r.Context().Err() != nil:
		h.logf("voice: dictation clip %.1f s: canceled during transcription after %d ms", seconds, ms)
		answerJSON(w, http.StatusServiceUnavailable, map[string]string{"error": WordInterrupted})
	case err != nil:
		h.logf("voice: dictation clip %.1f s: failed after %d ms: %v", seconds, ms, err)
		answerJSON(w, http.StatusBadGateway, map[string]string{"error": WordFailed})
	default:
		h.logf("voice: dictation clip %.1f s: transcribed in %d ms", seconds, ms)
		answerJSON(w, http.StatusOK, map[string]string{"text": text})
	}
}

// drainClip reads and drops the rest of a refused clip (bounded): the server
// closes the connection after the answer, and a client still writing the body
// would get a broken pipe instead of the refusal's word.
func drainClip(r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, MaxClipBytes(MaxMaxSeconds)))
}

type DictateClient struct {
	Socket string
	// HealthTTL is how long a health answer is reused; zero = 5 s.
	HealthTTL time.Duration
	// MaxWait bounds one transcription; zero = 10 minutes.
	MaxWait time.Duration

	once   sync.Once
	client *http.Client

	mu      sync.Mutex
	ready   bool
	checked time.Time
	probing chan struct{} // closed when the health probe running now ends
}

// dictateBase's host part is never resolved: the dialer always opens the
// socket.
const dictateBase = "http://lever-tool-whisper"

const maxDictateAnswer = 2 << 20

func (c *DictateClient) http() *http.Client {
	c.once.Do(func() {
		c.client = &http.Client{Transport: &http.Transport{
			// Never a proxy from the environment, never TCP: the socket.
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				if err := checkSocketOwner(c.Socket); err != nil {
					return nil, err
				}
				var d net.Dialer
				return d.DialContext(ctx, "unix", c.Socket)
			},
			// A fresh connection per request: a pooled one could be dead after a
			// tool restart, and a clip sent on it would fail without a refund.
			DisableKeepAlives: true,
		}}
	})
	return c.client
}

func (c *DictateClient) Health(ctx context.Context) (Health, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dictateBase+"/health", nil)
	if err != nil {
		return Health{}, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return Health{}, plainErr(err)
	}
	defer resp.Body.Close()
	var h Health
	if resp.StatusCode != http.StatusOK {
		return Health{}, fmt.Errorf("%w: health answered HTTP %d", ErrTool, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDictateAnswer)).Decode(&h); err != nil {
		return Health{}, fmt.Errorf("%w: health answer is not JSON", ErrTool)
	}
	return h, nil
}

// Available reports whether the tool's health route answers ready. The
// answer is reused for HealthTTL; once stale, one caller probes while the
// others keep the previous answer (before any answer, they wait for that
// probe). Probes never pile up.
func (c *DictateClient) Available() bool {
	ttl := c.HealthTTL
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	c.mu.Lock()
	if !c.checked.IsZero() && time.Since(c.checked) < ttl {
		r := c.ready
		c.mu.Unlock()
		return r
	}
	if wait := c.probing; wait != nil {
		if !c.checked.IsZero() {
			r := c.ready
			c.mu.Unlock()
			return r
		}
		c.mu.Unlock()
		<-wait
		c.mu.Lock()
		r := c.ready
		c.mu.Unlock()
		return r
	}
	done := make(chan struct{})
	c.probing = done
	c.mu.Unlock()
	ready := false
	defer func() {
		c.mu.Lock()
		c.ready, c.checked, c.probing = ready, time.Now(), nil
		c.mu.Unlock()
		close(done)
	}()
	h, err := c.Health(context.Background())
	ready = err == nil && h.Ready
	return ready
}

// Transcribe sends one checked clip to the tool. An error wraps ErrNotSent
// when the clip never reached whisper-server (the socket did not answer, or
// the tool refused it first). A clip whose caller gave up counts wherever it
// was. The error never carries the transcript.
func (c *DictateClient) Transcribe(ctx context.Context, wav []byte) (string, error) {
	wait := c.MaxWait
	if wait <= 0 {
		wait = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dictateBase+"/dictate", bytes.NewReader(wav))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "audio/wav")
	resp, err := c.http().Do(req)
	if err != nil {
		return "", plainErr(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDictateAnswer))
	if err != nil {
		return "", err
	}
	var ans struct {
		Text  *string `json:"text"`
		Error string  `json:"error"`
	}
	if err := json.Unmarshal(raw, &ans); err != nil {
		return "", fmt.Errorf("%w: HTTP %d, the answer is not JSON", ErrTool, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusOK && ans.Text != nil {
		return *ans.Text, nil
	}
	switch ans.Error {
	case WordNotReady:
		return "", fmt.Errorf("%w: %w", ErrNotSent, ErrNotReady)
	case WordBadAudio, WordTooLong:
		return "", fmt.Errorf("%w: lever-tool-whisper refused the clip (%s)", ErrNotSent, ans.Error)
	}
	// A fixed word only: the tool's answer is never quoted beyond it.
	word := ans.Error
	if word != WordFailed && word != WordInterrupted && word != WordCanceled {
		word = "an unknown answer"
	}
	return "", fmt.Errorf("%w: HTTP %d, %s", ErrTool, resp.StatusCode, word)
}

// checkSocketOwner refuses a socket whose directory is not a private
// directory of the caller's (another user could listen there) or that is not
// the caller's own socket. The clip is then not sent (ErrNotSent).
func checkSocketOwner(p string) error {
	if err := socketOwnerErr(p, "dictation socket"); err != nil {
		return fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	return nil
}

func socketOwnerErr(p, what string) error {
	dir := filepath.Dir(p)
	di, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("the %s's directory: %v", what, err)
	}
	if !di.IsDir() || di.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("the %s's directory %s is not a private directory (mode %v)", what, dir, di.Mode())
	}
	if owner, ok := fileOwner(di); ok && owner != os.Getuid() {
		return fmt.Errorf("the %s's directory %s belongs to uid %d, not to you", what, dir, owner)
	}
	si, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("the %s: %v", what, err)
	}
	if si.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("%s is not a socket", p)
	}
	if owner, ok := fileOwner(si); ok && owner != os.Getuid() {
		return fmt.Errorf("the %s %s belongs to uid %d, not to you", what, p, owner)
	}
	return nil
}

// plainErr drops the URL from a client error, and marks a failed dial as
// ErrNotSent.
func plainErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, ErrNotSent) {
		return err
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return fmt.Errorf("%w: the dictation socket does not answer: %v", ErrNotSent, err)
	}
	return err
}
