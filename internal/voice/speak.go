package voice

// The speech socket: how the remote proxy reaches lever-tool-tts.
//
// Like the dictation socket (dictate.go): a Unix socket (-speak-socket,
// remote.voice.speak_socket) in a private directory (0700, the
// operator's), the socket itself 0600, never on the broker gateway or a TCP
// port, so neither the jail nor another local user reaches it.
//
// Routes:
//
//	POST /speak   body: {"text": "...", "voice": "name"} (JSON, voice ""
//	              = the tool's -voice). 200 audio/wav (16-bit PCM, mono),
//	              or an error status with {"error": word}.
//	GET  /health  200 SpeakHealth.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Error words of the speech socket, beyond the dictation socket's
// WordNotReady, WordFailed and WordCanceled.
const (
	// WordBadText: nothing speakable in the text, or it is too long.
	WordBadText = "bad-text"
	// WordBadVoice: the model has no such voice.
	WordBadVoice = "bad-voice"
)

// maxSpeakBody bounds a /speak request: MaxSpeakMaxChars characters of
// four bytes each, escaped, with room to spare.
const maxSpeakBody = 64 << 10

// ListenSpeak creates the speech socket at path, as ListenDictate does the
// dictation socket.
func ListenSpeak(path string) (net.Listener, error) {
	return listenPrivate(path, "speech socket", "lever-tool-tts")
}

// SpeakVoice is one voice as the health answer lists it.
type SpeakVoice struct {
	Name string `json:"name"`
	Lang string `json:"lang"`
}

// SpeakHealth is the answer of GET /health.
type SpeakHealth struct {
	Ready    bool         `json:"ready"`
	Model    string       `json:"model"`
	Voice    string       `json:"voice"` // the default
	Voices   []SpeakVoice `json:"voices"`
	MaxChars int          `json:"maxChars"`
}

// SpeakRequest is the body of POST /speak.
type SpeakRequest struct {
	Text  string `json:"text"`
	Voice string `json:"voice,omitempty"`
}

// SpeechEngine is what the socket needs of Speaker.
type SpeechEngine interface {
	Ready() bool
	Speak(ctx context.Context, text, voice string) ([]byte, error)
}

// SpeakHandler serves the speech socket.
type SpeakHandler struct {
	Engine   SpeechEngine
	Model    SpeechModel
	Voice    string // the default
	MaxChars int
	// Log receives one line per request: its length, the voice, the
	// outcome and the latency; never the text, never the audio. nil drops
	// them.
	Log func(format string, a ...any)
}

func (h *SpeakHandler) logf(format string, a ...any) {
	if h.Log != nil {
		h.Log(format, a...)
	}
}

// Health is the handler's health answer.
func (h *SpeakHandler) Health() SpeakHealth {
	vs := make([]SpeakVoice, len(h.Model.Voices))
	for i, v := range h.Model.Voices {
		vs[i] = SpeakVoice{Name: v.Name, Lang: v.Lang}
	}
	return SpeakHealth{Ready: h.Engine.Ready(), Model: h.Model.Name, Voice: h.Voice, Voices: vs, MaxChars: h.MaxChars}
}

func (h *SpeakHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/health" && r.Method == http.MethodGet:
		answerJSON(w, http.StatusOK, h.Health())
	case r.URL.Path == "/speak" && r.Method == http.MethodPost:
		h.speak(w, r)
	case r.URL.Path == "/health" || r.URL.Path == "/speak":
		answerJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method"})
	default:
		answerJSON(w, http.StatusNotFound, map[string]string{"error": "not-found"})
	}
}

func (h *SpeakHandler) speak(w http.ResponseWriter, r *http.Request) {
	var req SpeakRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxSpeakBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		answerJSON(w, http.StatusBadRequest, map[string]string{"error": WordBadText})
		return
	}
	n := len([]rune(req.Text))
	voice := req.Voice
	if voice == "" {
		voice = h.Voice
	}
	if _, ok := h.Model.Voice(voice); !ok {
		h.logf("voice: speech request of %d characters: no voice %q", n, oneWord(voice))
		answerJSON(w, http.StatusBadRequest, map[string]string{"error": WordBadVoice})
		return
	}
	if _, err := SpeechInput(req.Text, h.MaxChars); err != nil {
		h.logf("voice: speech request of %d characters: refused (%v)", n, err)
		answerJSON(w, http.StatusBadRequest, map[string]string{"error": WordBadText})
		return
	}
	start := time.Now()
	wav, err := h.Engine.Speak(r.Context(), req.Text, voice)
	ms := time.Since(start).Milliseconds()
	switch {
	case err != nil && r.Context().Err() != nil:
		h.logf("voice: speech request of %d characters, voice %s: canceled after %d ms", n, voice, ms)
		answerJSON(w, http.StatusServiceUnavailable, map[string]string{"error": WordCanceled})
	case errors.Is(err, ErrSpeakNotSent):
		h.logf("voice: speech request of %d characters, voice %s: did not reach the engine: %v", n, voice, err)
		answerJSON(w, http.StatusServiceUnavailable, map[string]string{"error": WordNotReady})
	case err != nil:
		h.logf("voice: speech request of %d characters, voice %s: failed after %d ms: %v", n, voice, ms, err)
		answerJSON(w, http.StatusBadGateway, map[string]string{"error": WordFailed})
	default:
		h.logf("voice: speech request of %d characters, voice %s: %d bytes of audio in %d ms", n, voice, len(wav), ms)
		w.Header().Set("Content-Type", "audio/wav")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(wav)
	}
}

// oneWord is s for a log line when it is a short plain word, else "?".
func oneWord(s string) string {
	if len(s) > 40 {
		return "?"
	}
	for _, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return "?"
		}
	}
	return s
}

// NewSpeakServer is the http.Server for the speech socket.
func NewSpeakServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: time.Minute}
}

// SpeakClient is the remote proxy's side of the speech socket.
type SpeakClient struct {
	Socket string
	// HealthTTL is how long a health answer is reused; zero = 5 s.
	HealthTTL time.Duration
	// MaxWait bounds one request; zero = 2 minutes.
	MaxWait time.Duration

	once   sync.Once
	client *http.Client

	mu      sync.Mutex
	health  SpeakHealth
	ok      bool
	checked time.Time
	probing chan struct{}
}

// speakBase is the URL base of every request on the socket.
const speakBase = "http://lever-tool-tts"

func (c *SpeakClient) http() *http.Client {
	c.once.Do(func() {
		c.client = &http.Client{Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				if err := socketOwnerErr(c.Socket, "speech socket"); err != nil {
					return nil, fmt.Errorf("%w: %w", ErrSpeakNotSent, err)
				}
				var d net.Dialer
				return d.DialContext(ctx, "unix", c.Socket)
			},
			DisableKeepAlives: true,
		}}
	})
	return c.client
}

// Health asks the tool for its state.
func (c *SpeakClient) Health(ctx context.Context) (SpeakHealth, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, speakBase+"/health", nil)
	if err != nil {
		return SpeakHealth{}, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return SpeakHealth{}, speakPlainErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SpeakHealth{}, fmt.Errorf("lever-tool-tts: health answered HTTP %d", resp.StatusCode)
	}
	var h SpeakHealth
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDictateAnswer)).Decode(&h); err != nil {
		return SpeakHealth{}, errors.New("lever-tool-tts: health answer is not JSON")
	}
	return h, nil
}

// Available is the tool's health answer when it answers ready (ok false
// otherwise), reused for HealthTTL; one caller probes at a time, the
// others keep the previous answer (or, before any, wait for that probe).
func (c *SpeakClient) Available() (SpeakHealth, bool) {
	ttl := c.HealthTTL
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	c.mu.Lock()
	if !c.checked.IsZero() && time.Since(c.checked) < ttl {
		h, ok := c.health, c.ok
		c.mu.Unlock()
		return h, ok
	}
	if wait := c.probing; wait != nil {
		if c.checked.IsZero() {
			c.mu.Unlock()
			<-wait
			c.mu.Lock()
		}
		h, ok := c.health, c.ok
		c.mu.Unlock()
		return h, ok
	}
	done := make(chan struct{})
	c.probing = done
	c.mu.Unlock()
	h, err := c.Health(context.Background())
	ok := err == nil && h.Ready
	if !ok {
		h = SpeakHealth{}
	}
	c.mu.Lock()
	c.health, c.ok, c.checked, c.probing = h, ok, time.Now(), nil
	c.mu.Unlock()
	close(done)
	return h, ok
}

// Speak sends one text to the tool and returns the WAV it answers. An
// error wraps ErrSpeakNotSent when the text never reached the engine: the
// socket did not answer, or the tool refused it first. It never carries
// the text.
func (c *SpeakClient) Speak(ctx context.Context, text, voice string) ([]byte, error) {
	wait := c.MaxWait
	if wait <= 0 {
		wait = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	body, err := json.Marshal(SpeakRequest{Text: text, Voice: voice})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, speakBase+"/speak", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, speakPlainErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && resp.Header.Get("Content-Type") == "audio/wav" {
		wav, err := io.ReadAll(io.LimitReader(resp.Body, maxSpeechFile(48000)+1))
		if err != nil {
			return nil, err
		}
		if int64(len(wav)) > maxSpeechFile(48000) {
			return nil, errors.New("lever-tool-tts: the audio is too large")
		}
		return wav, nil
	}
	var ans struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, maxDictateAnswer)).Decode(&ans)
	switch ans.Error {
	case WordNotReady, WordBadText, WordBadVoice:
		return nil, fmt.Errorf("%w: lever-tool-tts refused it (%s)", ErrSpeakNotSent, ans.Error)
	case WordFailed, WordCanceled:
		return nil, fmt.Errorf("lever-tool-tts: HTTP %d, %s", resp.StatusCode, ans.Error)
	}
	return nil, fmt.Errorf("lever-tool-tts: HTTP %d, an unknown answer", resp.StatusCode)
}

// speakPlainErr drops the URL from a client error, and marks a failed dial
// as ErrSpeakNotSent.
func speakPlainErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, ErrSpeakNotSent) {
		return err
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return fmt.Errorf("%w: the speech socket does not answer: %v", ErrSpeakNotSent, err)
	}
	return err
}
