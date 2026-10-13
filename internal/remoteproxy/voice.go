package remoteproxy

// Dictation (remote.voice): POST /lever/api/voice/transcribe turns one clip
// into text for the page's message box; nothing is sent to an agent or stored.
// The clip stays in memory and goes to lever-tool-whisper (package voice);
// the text returns only to the login that sent it. The audit line never
// carries audio or text. Guards match the upload route (files.go): a custom
// header, per-login rates, slots of which a contact never takes the last, and
// a size-scaled body deadline.

import (
	"bytes"
	"context"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/chatledger"
	"github.com/stevegeek/lever/internal/voice"
)

// VoiceConfig is remote.voice as the proxy needs it.
type VoiceConfig struct {
	MaxSeconds int
	// Excluded are the logins with voice: false: no mic, and the route
	// answers voice-off.
	Excluded []string
	// Available reports whether lever-tool-whisper answers ready (cached briefly).
	Available func() bool
	// NotSent reports whether a Transcribe error means the clip never reached
	// the tool's queue (voice.ErrNotSent); only then are the limits given back.
	// nil = never.
	NotSent func(error) bool
	// ReadAloud offers read-aloud to logins with voice on (device or server).
	ReadAloud bool
	// Speech is the server voice (read_aloud: server); nil = device voices only.
	Speech *SpeechConfig
	// Transcribe turns one checked clip (WAV, PCM s16le, 16 kHz, mono) into
	// text. Its error goes to the audit line, so it must never carry the
	// transcript.
	Transcribe func(ctx context.Context, wav []byte) (string, error)
}

const (
	DecisionVoice     Decision = "voice-transcribe"
	DecisionDenyVoice Decision = "deny-voice"
)

const (
	voicePrefix = "/lever/api/voice/"
	voicePath   = voicePrefix + "transcribe"
	// voiceHeader must be on every clip, like X-Lever-Upload: a custom
	// header crosses origins only after a CORS preflight, which the proxy
	// never grants.
	voiceHeader = "X-Lever-Voice"
	// One clip on the GPU at a time, and two waiting: voiceSlots in all, of
	// which a contact never takes the last, so the operator can always
	// dictate. A login has one clip in flight at most.
	voiceSlots    = 3
	voicePerLogin = 1
	// Per login, in memory: clips transcribed per hour, audio per day, and
	// attempts (refused ones included) per hour.
	voiceClipsPerHour  = 30
	voiceSecondsPerDay = 60 * 60
	voiceTriesPerHour  = 2 * voiceClipsPerHour
	// voiceBodyBase plus the body at fileBodyMinRate, at most voiceBodyMax:
	// about 3.5 min for a 300 s clip.
	voiceBodyBase = time.Minute
	voiceBodyMax  = 30 * time.Minute
)

type voiceState struct {
	cfg      VoiceConfig
	now      func() time.Time // tests
	deadline time.Duration    // tests: the body deadline (0 = bodyDeadline)

	mu    sync.Mutex
	total int
	held  map[string]int        // lowercase login → clips in flight
	used  map[string][]voiceUse // lowercase login → clips of the last day
	gpu   chan struct{}         // one transcription at a time
	tries *loginRate
}

type voiceUse struct {
	at      time.Time
	seconds float64
}

func newVoiceState(cfg VoiceConfig) *voiceState {
	return &voiceState{cfg: cfg, now: time.Now, held: map[string]int{}, used: map[string][]voiceUse{},
		gpu: make(chan struct{}, 1), tries: newLoginRate(voiceTriesPerHour, time.Hour)}
}

func (g *gate) voiceOnFor(login string) bool {
	if g.voice == nil {
		return false
	}
	for _, x := range g.voice.cfg.Excluded {
		if sameLogin(x, login) {
			return false
		}
	}
	return true
}

type voiceInfo struct {
	MaxSeconds int `json:"maxSeconds"`
}

// readAloudFor does not depend on the transcriber: read-aloud runs on the
// device or lever-tool-tts.
func (g *gate) readAloudFor(login string) bool {
	return g.voiceOnFor(login) && g.voice.cfg.ReadAloud
}

func (g *gate) voiceInfoFor(login string) *voiceInfo {
	if !g.voiceOnFor(login) || g.voice.cfg.Available == nil || !g.voice.cfg.Available() {
		return nil
	}
	return &voiceInfo{MaxSeconds: g.voice.cfg.MaxSeconds}
}

func (s *voiceState) maxBody() int64 {
	return int64(voice.BytesPerSecond)*int64(s.cfg.MaxSeconds) + 1024
}

func (s *voiceState) bodyDeadline() time.Duration {
	return min(voiceBodyBase+time.Duration(s.maxBody()/fileBodyMinRate)*time.Second, voiceBodyMax)
}

// begin: a login holds at most voicePerLogin slots, and only an operator
// takes the last of voiceSlots.
func (s *voiceState) begin(login string, operator bool) (func(), bool) {
	key := strings.ToLower(login)
	s.mu.Lock()
	defer s.mu.Unlock()
	free := voiceSlots - s.total
	if free <= 0 || !operator && free <= 1 || s.held[key] >= voicePerLogin {
		return nil, false
	}
	s.total++
	s.held[key]++
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.total--
		if s.held[key]--; s.held[key] <= 0 {
			delete(s.held, key)
		}
	}, true
}

// limit returns the refusal word for one more clip ("" = allowed); with take
// it also counts the clip.
func (s *voiceState) limit(login string, seconds float64, take bool) string {
	return s.check(login, seconds, s.now(), take)
}

func (s *voiceState) limitAt(login string, seconds float64, now time.Time) string {
	return s.check(login, seconds, now, true)
}

func (s *voiceState) check(login string, seconds float64, now time.Time, take bool) string {
	key := strings.ToLower(login)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, us := range s.used {
		keep := us[:0]
		for _, u := range us {
			if now.Sub(u.at) < 24*time.Hour {
				keep = append(keep, u)
			}
		}
		if len(keep) == 0 {
			delete(s.used, k)
		} else {
			s.used[k] = keep
		}
	}
	n, total := 0, seconds
	for _, u := range s.used[key] {
		if now.Sub(u.at) < time.Hour {
			n++
		}
		total += u.seconds
	}
	switch {
	case n >= voiceClipsPerHour:
		return "rate"
	case total > voiceSecondsPerDay:
		return "quota"
	}
	if take {
		s.used[key] = append(s.used[key], voiceUse{at: now, seconds: seconds})
	}
	return ""
}

// refund gives back a use for a clip the tool never took (the browser gave up
// waiting for the GPU token, or NotSent). A clip the tool took counts however
// it ended; voiceTriesPerHour counts every clip.
func (s *voiceState) refund(login string, seconds float64, at time.Time) {
	key := strings.ToLower(login)
	s.mu.Lock()
	defer s.mu.Unlock()
	us := s.used[key]
	for i := len(us) - 1; i >= 0; i-- {
		if us[i].at.Equal(at) && us[i].seconds == seconds {
			s.used[key] = append(us[:i], us[i+1:]...)
			return
		}
	}
}

func voiceAudio(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && (mt == "audio/wav" || mt == "audio/wave" || mt == "audio/x-wav")
}

func (g *gate) refuseVoice(w http.ResponseWriter, r *http.Request, line *AuditLine, status int, word string) {
	line.Reason = word
	g.answerFileJSON(w, r, line, DecisionDenyVoice, status, map[string]any{"error": word})
}

func (g *gate) serveVoice(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer) {
	s := g.voice
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		g.refuseVoice(w, r, line, http.StatusMethodNotAllowed, "method")
		return
	}
	if !g.voiceOnFor(v.login) {
		g.refuseVoice(w, r, line, http.StatusForbidden, "voice-off")
		return
	}
	if !sameOriginWrite(r, g.cfg.ServeHost) || r.Header.Get(voiceHeader) != "1" {
		g.refuseVoice(w, r, line, http.StatusForbidden, "origin")
		return
	}
	if !s.tries.take(v.login, s.now()) {
		w.Header().Set("Retry-After", "600")
		g.refuseVoice(w, r, line, http.StatusTooManyRequests, "rate")
		return
	}
	if s.cfg.Available == nil || !s.cfg.Available() || s.cfg.Transcribe == nil {
		line.Error = "lever-tool-whisper is not ready (see lever doctor and the tool log)"
		g.refuseVoice(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if !voiceAudio(r.Header.Get("Content-Type")) {
		g.refuseVoice(w, r, line, http.StatusUnsupportedMediaType, "bad-audio")
		return
	}
	limit := s.maxBody()
	if r.ContentLength > limit {
		g.refuseVoice(w, r, line, http.StatusRequestEntityTooLarge, "too-long")
		return
	}
	if word := s.limit(v.login, 0, false); word != "" {
		w.Header().Set("Retry-After", "600")
		g.refuseVoice(w, r, line, http.StatusTooManyRequests, word)
		return
	}
	done, ok := s.begin(v.login, v.tier == chatledger.TierOperator)
	if !ok {
		w.Header().Set("Retry-After", "10")
		g.refuseVoice(w, r, line, http.StatusTooManyRequests, "busy")
		return
	}
	defer done()
	// The server has no ReadTimeout (serve.go): the body gets its own. The
	// clip is read into memory only: never a file.
	rc := http.NewResponseController(w)
	deadline := s.bodyDeadline()
	if s.deadline > 0 {
		deadline = s.deadline
	}
	_ = rc.SetReadDeadline(time.Now().Add(deadline))
	var buf bytes.Buffer
	if r.ContentLength > 0 {
		buf.Grow(int(r.ContentLength))
	}
	_, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, limit))
	// The deadline is for the upload, not the GPU wait. net/http lifts it at EOF;
	// lifting it here also covers a body cut short.
	_ = rc.SetReadDeadline(time.Time{})
	switch {
	case tooLarge(err):
		g.refuseVoice(w, r, line, http.StatusRequestEntityTooLarge, "too-long")
		return
	case timedOut(err):
		g.refuseVoice(w, r, line, http.StatusRequestTimeout, "timeout")
		return
	case err != nil:
		g.refuseVoice(w, r, line, http.StatusBadRequest, "bad-audio")
		return
	}
	clip := buf.Bytes()
	seconds, word := voice.CheckWAV(clip, s.cfg.MaxSeconds)
	if word == "too-long" {
		g.refuseVoice(w, r, line, http.StatusRequestEntityTooLarge, word)
		return
	}
	if word != "" {
		g.refuseVoice(w, r, line, http.StatusBadRequest, word)
		return
	}
	line.AudioSeconds = float64(int64(seconds*10)) / 10
	takenAt := s.now()
	if word := s.limitAt(v.login, seconds, takenAt); word != "" {
		w.Header().Set("Retry-After", "600")
		g.refuseVoice(w, r, line, http.StatusTooManyRequests, word)
		return
	}
	// Wait for the GPU: a queued clip holds its slot meanwhile. A browser
	// that gives up ends the wait, and the clip does not count.
	select {
	case s.gpu <- struct{}{}:
	case <-r.Context().Done():
		s.refund(v.login, seconds, takenAt)
		line.Error = "the request ended while it waited for its turn"
		g.refuseVoice(w, r, line, http.StatusServiceUnavailable, "busy")
		return
	}
	start := time.Now()
	// A browser that gives up cancels the tool request, which dequeues the clip
	// or stops whisper-server on it, freeing the GPU token.
	text, err := s.cfg.Transcribe(r.Context(), clip)
	<-s.gpu
	line.LatencyMS = max(time.Since(start).Milliseconds(), 1)
	if err != nil {
		// Only a clip the tool never took (NotSent) is given back.
		if s.cfg.NotSent != nil && s.cfg.NotSent(err) {
			s.refund(v.login, seconds, takenAt)
		}
		line.Error = "transcription failed: " + err.Error()
		g.refuseVoice(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	g.answerFileJSON(w, r, line, DecisionVoice, http.StatusOK, map[string]any{"text": voice.CleanText(text)})
}
