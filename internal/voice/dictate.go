package voice

// The dictation socket: how the remote proxy reaches lever-tool-whisper.
//
// The tool listens on a Unix socket (-dictate-socket, remote.voice.socket)
// in a private directory (0700, the operator's), the socket itself 0600:
// only processes of the operator's user reach it, and the jail never does
// (it reaches no host Unix socket, and config load refuses the path inside
// the tree). Dictation deliberately does not travel the broker gateway or
// the tool's TCP backend: the gateway forwards every path under
// /mcp/<tool>/ to the backend, so a host-only route there would be one step
// from agents.
//
// Routes:
//
//	POST /dictate  body: one WAV clip (CheckWAV). When the clip leaves the
//	               queue and its transcription starts, an informational
//	               102 Processing answer (StatusStarted); then 200
//	               {"text": "..."} (the transcript as whisper-server gave
//	               it), or an error status with {"error": word}, word one of
//	               the Word* constants. The 102 tells the client whether a
//	               clip it gives up on had started (counted) or was still
//	               queued (not sent).
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
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	// queue; it never reached whisper-server (not sent).
	WordCanceled = "canceled"
	// WordInterrupted: the caller went away while whisper-server worked on
	// the clip (sent: its transcription had started).
	WordInterrupted = "interrupted"
)

// StatusStarted is the informational answer the dictation socket sends when
// a clip's transcription starts.
const StatusStarted = http.StatusProcessing

// MaxSocketPath bounds the socket path: sun_path holds 104 bytes on macOS
// and 108 on Linux, the terminating NUL included.
const MaxSocketPath = 103

// ErrTool: lever-tool-whisper answered, but not with a transcript.
var ErrTool = errors.New("lever-tool-whisper")

// CheckSocketPath accepts a clean absolute path short enough for a Unix
// socket. Whether it lies outside the tree is config load's check (and the
// tool's own, for -dictate-socket).
func CheckSocketPath(p string) error {
	switch {
	case !filepath.IsAbs(p) || filepath.Clean(p) != p:
		return fmt.Errorf("socket %q must be a clean absolute path", p)
	case len(p) > MaxSocketPath:
		return fmt.Errorf("socket %q is %d bytes; a Unix socket path holds at most %d", p, len(p), MaxSocketPath)
	}
	return nil
}

// ListenDictate creates the dictation socket at path: its directory is
// created 0700 when missing and must be a private directory of the caller's
// (PrivateDir: not a symbolic link, not reachable by other users); a stale
// socket left by a killed tool is removed, but only a socket of the
// caller's that nothing answers on; anything else at path is refused. The
// socket is 0600.
func ListenDictate(path string) (net.Listener, error) {
	if err := CheckSocketPath(path); err != nil {
		return nil, err
	}
	if err := PrivateDir(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("dictation socket directory: %w", err)
	}
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	case fi.Mode()&fs.ModeSocket == 0:
		return nil, fmt.Errorf("dictation socket %s: something that is not a socket is there (%s); lever leaves it alone", path, fi.Mode().Type())
	default:
		if owner, ok := fileOwner(fi); ok && owner != os.Getuid() {
			return nil, fmt.Errorf("dictation socket %s belongs to uid %d, not to you", path, owner)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("dictation socket %s is in use: another lever-tool-whisper serves it", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("dictation socket %s: removing the stale socket: %w", path, err)
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

// Transcriber is what the socket and the agent operation need of Service.
type Transcriber interface {
	Ready() bool
	Transcribe(ctx context.Context, wav []byte) (string, error)
}

// Health is the answer of GET /health.
type Health struct {
	Ready      bool   `json:"ready"`
	Model      string `json:"model"`
	MaxSeconds int    `json:"maxSeconds"`
}

// DictateHandler serves the dictation socket.
type DictateHandler struct {
	Svc        Transcriber
	Queue      *Queue
	Model      string
	MaxSeconds int
	// Log receives one line per clip: its length, the outcome and the
	// latency; never the audio, never the text. nil drops them.
	Log func(format string, a ...any)
}

// NewDictateServer is the http.Server for the socket: header and body
// deadlines, no write deadline (a transcription takes what it takes;
// Service bounds it).
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
	// From here the clip counts: tell the client before whisper-server
	// sees it (1xx headers are sent at once).
	w.WriteHeader(StatusStarted)
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

// drainClip reads and drops the rest of a refused clip, at most the largest
// clip any tool takes: the server closes the connection after the answer
// (the client asks for that), and a client still writing the body would
// otherwise get a broken pipe instead of the refusal's word.
func drainClip(r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, MaxClipBytes(MaxMaxSeconds)))
}

// DictateClient is the remote proxy's side of the socket.
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

// dictateBase is the URL base of every request on the socket (the host part
// is never resolved: the dialer always opens the socket).
const dictateBase = "http://lever-tool-whisper"

// maxDictateAnswer bounds an answer the client reads.
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
			// A fresh connection per request: a pooled one could be dead
			// after a tool restart, and a clip sent on it would fail
			// without being given back.
			DisableKeepAlives: true,
		}}
	})
	return c.client
}

// Health asks the tool for its state.
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

// Available reports whether the tool answers its health route ready. The
// answer is reused for HealthTTL, so the chat roster does not ask the tool
// on every refresh. Once it is stale, one caller asks (a probe takes up to
// 2 s) while the others keep the previous answer; before any answer, they
// wait for that one probe. Probes never pile up.
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
// when the clip never reached whisper-server: the socket did not answer,
// the tool refused it before whisper-server (not ready, or the clip failed
// the tool's own check), or ctx ended (the browser gave up, or MaxWait ran
// out) while the clip still waited in the tool's queue, before the tool
// reported its transcription started (StatusStarted). Once started, a clip
// that ends early is not ErrNotSent. The error never carries the
// transcript.
func (c *DictateClient) Transcribe(ctx context.Context, wav []byte) (string, error) {
	wait := c.MaxWait
	if wait <= 0 {
		wait = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var started atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
			if code == StatusStarted {
				started.Store(true)
			}
			return nil
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dictateBase+"/dictate", bytes.NewReader(wav))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "audio/wav")
	resp, err := c.http().Do(req)
	if err != nil {
		if ctx.Err() != nil && !started.Load() {
			return "", fmt.Errorf("%w: canceled while it waited in the tool's queue (%v)", ErrNotSent, ctx.Err())
		}
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
	case WordCanceled:
		return "", fmt.Errorf("%w: canceled while it waited in the tool's queue", ErrNotSent)
	}
	// A fixed word only: the tool's answer is never quoted beyond it.
	word := ans.Error
	if word != WordFailed && word != WordInterrupted {
		word = "an unknown answer"
	}
	return "", fmt.Errorf("%w: HTTP %d, %s", ErrTool, resp.StatusCode, word)
}

// checkSocketOwner refuses a socket the proxy should not send audio to: one
// whose directory is not a private directory of the caller's (another user
// could have made it and listen there), or that is not the caller's own
// socket. The clip is then not sent (ErrNotSent).
func checkSocketOwner(p string) error {
	dir := filepath.Dir(p)
	di, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("%w: the dictation socket's directory: %v", ErrNotSent, err)
	}
	if !di.IsDir() || di.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: the dictation socket's directory %s is not a private directory (mode %v)", ErrNotSent, dir, di.Mode())
	}
	if owner, ok := fileOwner(di); ok && owner != os.Getuid() {
		return fmt.Errorf("%w: the dictation socket's directory %s belongs to uid %d, not to you", ErrNotSent, dir, owner)
	}
	si, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("%w: the dictation socket: %v", ErrNotSent, err)
	}
	if si.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("%w: %s is not a socket", ErrNotSent, p)
	}
	if owner, ok := fileOwner(si); ok && owner != os.Getuid() {
		return fmt.Errorf("%w: the dictation socket %s belongs to uid %d, not to you", ErrNotSent, p, owner)
	}
	return nil
}

// plainErr drops the URL from a client error, and marks a failed dial (the
// socket missing, or nothing listening) as ErrNotSent.
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
