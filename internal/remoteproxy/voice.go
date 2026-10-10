package remoteproxy

// Dictation on the chat page (remote.voice): POST /lever/api/voice/transcribe
// takes one recorded clip and answers its text, which the page puts into the
// message box for the login to review and send itself. Nothing is sent to an
// agent here, and nothing is stored: the clip is read into memory, checked,
// handed to the transcriber (the broker tool lever-tool-whisper, over its
// dictation socket: package voice) and dropped; the text goes back only to
// the login that sent the clip. The audit line has the login, the clip's length, the outcome and the
// latency; never the audio, never the text.
//
// The shape follows the upload route (files.go): the login gate, a custom
// header no other site can send, per-login rates, slots of which a contact
// never takes the last, and a body deadline that scales with the size.

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
	// MaxSeconds bounds one clip.
	MaxSeconds int
	// Excluded are the logins with voice: false: no mic, and the route
	// answers voice-off.
	Excluded []string
	// Available reports whether dictation can run: lever-tool-whisper
	// answers its health route ready (voice.DictateClient, cached briefly).
	// False: the roster carries no voice and the route answers unavailable.
	Available func() bool
	// NotSent reports whether a Transcribe error means the clip never
	// reached whisper-server (voice.ErrNotSent: the socket did not answer,
	// the tool refused the clip before whisper-server, or the request ended
	// while the clip waited in the tool's queue, before the tool reported
	// its transcription started): only then is its use of the limits given
	// back. nil = never.
	NotSent func(error) bool
	// ReadAloud: the roster tells the page to offer read-aloud to every
	// login with voice on (remote.voice.read_aloud).
	ReadAloud bool
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

// voiceState holds the route's slots and per-login counts.
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

// voiceOnFor reports whether login may dictate: voice on and the login not
// excluded.
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

// voiceInfo is the roster's voice entry: shown only when the login may
// dictate and the transcriber is up.
type voiceInfo struct {
	MaxSeconds int `json:"maxSeconds"`
}

// readAloudFor reports whether the page offers read-aloud to login: voice
// on for it and remote.voice.read_aloud not turned off. Independent of the
// transcriber: read-aloud runs on the device.
func (g *gate) readAloudFor(login string) bool {
	return g.voiceOnFor(login) && g.voice.cfg.ReadAloud
}

func (g *gate) voiceInfoFor(login string) *voiceInfo {
	if !g.voiceOnFor(login) || g.voice.cfg.Available == nil || !g.voice.cfg.Available() {
		return nil
	}
	return &voiceInfo{MaxSeconds: g.voice.cfg.MaxSeconds}
}

// maxBody is the largest body a clip of MaxSeconds can be.
func (s *voiceState) maxBody() int64 {
	return int64(voice.BytesPerSecond)*int64(s.cfg.MaxSeconds) + 1024
}

func (s *voiceState) bodyDeadline() time.Duration {
	return min(voiceBodyBase+time.Duration(s.maxBody()/fileBodyMinRate)*time.Second, voiceBodyMax)
}

// begin takes a slot for login, or reports false: a login holds at most
// voicePerLogin, and only an operator takes the last of voiceSlots.
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

// limit is the refusal word for one more clip of seconds by login ("" =
// allowed), and with take it also counts the clip. Uses older than a day
// are forgotten.
func (s *voiceState) limit(login string, seconds float64, take bool) string {
	return s.check(login, seconds, s.now(), take)
}

// limitAt is limit with take, recording the use at now (refund finds it).
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

// refund gives back a use limit took (take true) at the same moment for the
// same length, for a clip that never reached whisper-server: the browser
// gave up while it waited for the proxy's GPU token, or Transcribe's error
// is NotSent (the socket did not answer, the tool refused the clip, or the
// browser gave up while it waited in the tool's queue, before the tool's
// signal that its transcription started reached the proxy). A clip whose
// start signal arrived is never given back, even when it failed, timed out
// or the browser gave up. (A cancel in the microseconds between the tool
// sending that signal and the proxy reading it is given back.) The attempt cap (voiceTriesPerHour) still counts
// it.
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

// voiceAudio reports whether a Content-Type names a WAV body.
func voiceAudio(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && (mt == "audio/wav" || mt == "audio/wave" || mt == "audio/x-wav")
}

// refuseVoice answers the route with one fixed error word.
func (g *gate) refuseVoice(w http.ResponseWriter, r *http.Request, line *AuditLine, status int, word string) {
	line.Reason = word
	g.answerFileJSON(w, r, line, DecisionDenyVoice, status, map[string]any{"error": word})
}

// serveVoice answers POST /lever/api/voice/transcribe for v.
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
	// The body is in: the deadline is for the upload, not for the wait for
	// the GPU or the transcription. net/http lifts it itself when the body
	// hits EOF; lifting it here too keeps that true for a body cut short.
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
	// A browser that gives up ends the request to the tool: a clip still in
	// the tool's queue leaves it (not sent), and one being transcribed ends
	// the tool's request to whisper-server, which stops work on it; the GPU
	// token is freed with it.
	text, err := s.cfg.Transcribe(r.Context(), clip)
	<-s.gpu
	line.LatencyMS = max(time.Since(start).Milliseconds(), 1)
	if err != nil {
		// Only a clip that never reached whisper-server is given back
		// (NotSent, a clip canceled in the tool's queue included): one whose
		// transcription started used it, failed, timed out or canceled.
		if s.cfg.NotSent != nil && s.cfg.NotSent(err) {
			s.refund(v.login, seconds, takenAt)
		}
		line.Error = "transcription failed: " + err.Error()
		g.refuseVoice(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	g.answerFileJSON(w, r, line, DecisionVoice, http.StatusOK, map[string]any{"text": voice.CleanText(text)})
}
