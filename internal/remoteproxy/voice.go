package remoteproxy

// Dictation on the chat page (remote.voice): POST /lever/api/voice/transcribe
// takes one recorded clip and answers its text, which the page puts into the
// message box for the login to review and send itself. Nothing is sent to an
// agent here, and nothing is stored: the clip is read into memory, checked,
// handed to the transcriber (a whisper.cpp server on host loopback, package
// voice) and dropped; the text goes back only to the login that sent the
// clip. The audit line has the login, the clip's length, the outcome and the
// latency; never the audio, never the text.
//
// The shape follows the upload route (files.go): the login gate, a custom
// header no other site can send, per-login rates, slots of which a contact
// never takes the last, and a body deadline that scales with the size.

import (
	"bytes"
	"context"
	"encoding/binary"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/stevegeek/lever/internal/chatledger"
)

// VoiceConfig is remote.voice as the proxy needs it.
type VoiceConfig struct {
	// MaxSeconds bounds one clip.
	MaxSeconds int
	// Excluded are the logins with voice: false: no mic, and the route
	// answers voice-off.
	Excluded []string
	// Available reports whether dictation can run: the model and program
	// passed their checks at start and the child is supervised. False: the
	// roster carries no voice and the route answers unavailable.
	Available func() bool
	// NotSent reports whether a Transcribe error means the clip never
	// reached the transcriber (voice.ErrNotSent): only then is its use of
	// the limits given back. nil = never.
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
	voiceClipsPerHour   = 30
	voiceSecondsPerDay  = 60 * 60
	voiceTriesPerHour   = 2 * voiceClipsPerHour
	voiceSampleRate     = 16000
	voiceBytesPerSecond = 2 * voiceSampleRate // s16le mono
	wavHeaderLen        = 44
	// voiceMinBytes is a tenth of a second: anything shorter holds no word.
	voiceMinBytes = voiceBytesPerSecond / 10
	// voiceBodyBase plus the body at fileBodyMinRate, at most voiceBodyMax:
	// about 3.5 min for a 300 s clip.
	voiceBodyBase = time.Minute
	voiceBodyMax  = 30 * time.Minute
	// voiceMaxText is the chat message limit (scion's
	// messages.MaxMessageLength, chatcore.js MAX_MESSAGE), in characters.
	voiceMaxText = 16000
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
	return int64(voiceBytesPerSecond)*int64(s.cfg.MaxSeconds) + 1024
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
// same length: the clip never reached the transcriber, or the transcriber
// failed. The attempt cap (voiceTriesPerHour) still counts it.
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

// checkWAV checks a clip: the canonical 44-byte header of PCM s16le, 16 kHz,
// mono (what the page encodes), every header field exact, the sizes
// matching the body, at least voiceMinBytes of samples and at most
// maxSeconds. whisper.cpp then parses only a header lever has checked byte
// for byte. It answers the clip's length, or a refusal word.
func checkWAV(b []byte, maxSeconds int) (float64, string) {
	if len(b) < wavHeaderLen+voiceMinBytes {
		return 0, "bad-audio"
	}
	le := binary.LittleEndian
	data := len(b) - wavHeaderLen
	ok := string(b[0:4]) == "RIFF" && le.Uint32(b[4:8]) == uint32(len(b)-8) && string(b[8:12]) == "WAVE" &&
		string(b[12:16]) == "fmt " && le.Uint32(b[16:20]) == 16 &&
		le.Uint16(b[20:22]) == 1 && // PCM
		le.Uint16(b[22:24]) == 1 && // mono
		le.Uint32(b[24:28]) == voiceSampleRate &&
		le.Uint32(b[28:32]) == voiceBytesPerSecond &&
		le.Uint16(b[32:34]) == 2 && // block align
		le.Uint16(b[34:36]) == 16 && // bits per sample
		string(b[36:40]) == "data" && le.Uint32(b[40:44]) == uint32(data) && data%2 == 0
	if !ok {
		return 0, "bad-audio"
	}
	if data > voiceBytesPerSecond*maxSeconds {
		return 0, "too-long"
	}
	return float64(data) / voiceBytesPerSecond, ""
}

// voiceAudio reports whether a Content-Type names a WAV body.
func voiceAudio(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && (mt == "audio/wav" || mt == "audio/wave" || mt == "audio/x-wav")
}

// voiceText is the transcript as the page gets it: trimmed, and cut to the
// chat message limit.
func voiceText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	// Whisper writes non-speech as markers ([BLANK_AUDIO], [MUSIC],
	// (silence)); they are not words the login said. Only known ones are
	// removed, so dictated code such as a[i] or [TODO] stays.
	if whisperMarker.MatchString(s) {
		s = strings.TrimSpace(spaceRun.ReplaceAllString(whisperMarker.ReplaceAllString(s, " "), " "))
	}
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= voiceMaxText {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:voiceMaxText]))
}

// whisperMarker is one of Whisper's known non-speech markers, in square
// brackets or parentheses, with the spaces around it.
var whisperMarker = regexp.MustCompile(`(?i)[ \t]*[\[(]\s*(blank_audio|music|silence|noise|inaudible|applause|laughter|sound effect|sound-effect|no speech)\s*[\])][ \t]*`)

// spaceRun is a run of spaces or tabs (line breaks are kept).
var spaceRun = regexp.MustCompile(`[ \t]{2,}`)

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
		line.Error = "the transcriber is not running (see the proxy log)"
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
	seconds, word := checkWAV(clip, s.cfg.MaxSeconds)
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
	// A browser that gives up ends the request to the child, which stops
	// work on the clip; the GPU token is freed with it.
	text, err := s.cfg.Transcribe(r.Context(), clip)
	<-s.gpu
	line.LatencyMS = max(time.Since(start).Milliseconds(), 1)
	if err != nil {
		// Only a clip that never reached the transcriber is given back: a
		// timeout or a failed transcription used it.
		if s.cfg.NotSent != nil && s.cfg.NotSent(err) {
			s.refund(v.login, seconds, takenAt)
		}
		line.Error = "transcription failed: " + err.Error()
		g.refuseVoice(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	g.answerFileJSON(w, r, line, DecisionVoice, http.StatusOK, map[string]any{"text": voiceText(text)})
}
