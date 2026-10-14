package remoteproxy

// Read-aloud with a server voice (read_aloud: server): POST
// /lever/api/voice/speak speaks one part of an agent message as a WAV via
// lever-tool-tts (package voice). Nothing is stored; the audio returns only to
// the login that asked, and the audit line never carries text or audio.
//
// The route takes the text from the page rather than looking it up: a lookup
// would have to repeat every visibility rule (a contact's filtered history
// above all), and a mistake would read out a message the login may not see.
// Since any text a login sends is spoken, the route is bounded per request,
// per login (characters an hour and a day), by the roster (the reference must
// name a listed agent) and by voice: false.

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/stevegeek/lever/internal/chatledger"
	"github.com/stevegeek/lever/internal/voice"
)

// SpeechConfig is read-aloud through lever-tool-tts, as the proxy needs it.
type SpeechConfig struct {
	// Available reports whether lever-tool-tts answers ready (cached briefly).
	Available func() (voice.SpeakHealth, bool)
	// Speak turns text into a WAV with voice ("" = default). Its error is
	// audited, so it must never carry the text.
	Speak func(ctx context.Context, text, voice string) ([]byte, error)
	// NotSent reports whether a Speak error means the text never reached the
	// engine (voice.ErrSpeakNotSent); only then are the limits given back.
	NotSent func(error) bool
}

const (
	DecisionSpeak     Decision = "voice-speak"
	DecisionDenySpeak Decision = "deny-speak"
)

const (
	speakPath = voicePrefix + "speak"
	// speakBody bounds a request: the tool's largest -max-chars of
	// four-byte characters, escaped, and the reference.
	speakBody = 64 << 10
	// One text with the engine at a time and two waiting; a contact never takes
	// the last of speakSlots, so the operator can always listen. A login has one
	// request in flight (the page fetches a part while the previous one plays).
	speakSlots    = 3
	speakPerLogin = 1
	// Per login, in memory: characters spoken an hour and a day, and
	// requests (refused ones included) an hour.
	speakCharsPerHour = 40000
	speakCharsPerDay  = 200000
	speakTriesPerHour = 1200
	speakBodyWait     = 30 * time.Second
	maxMessageRef     = 128
)

type speakState struct {
	cfg SpeechConfig
	now func() time.Time // tests

	mu    sync.Mutex
	total int
	held  map[string]int        // lowercase login → requests in flight
	used  map[string][]speakUse // lowercase login → texts of the last day
	run   chan struct{}         // one text with the engine at a time
	tries *loginRate
}

type speakUse struct {
	at    time.Time
	chars int
}

func newSpeakState(cfg SpeechConfig) *speakState {
	return &speakState{cfg: cfg, now: time.Now, held: map[string]int{}, used: map[string][]speakUse{},
		run: make(chan struct{}, 1), tries: newLoginRate(speakTriesPerHour, time.Hour)}
}

func (s *speakState) begin(login string, operator bool) (func(), bool) {
	key := strings.ToLower(login)
	s.mu.Lock()
	defer s.mu.Unlock()
	free := speakSlots - s.total
	if free <= 0 || !operator && free <= 1 || s.held[key] >= speakPerLogin {
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

// take counts chars for login, or returns "rate" (hour) or "quota" (day).
func (s *speakState) take(login string, chars int, now time.Time) string {
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
	hour, day := chars, chars
	for _, u := range s.used[key] {
		if now.Sub(u.at) < time.Hour {
			hour += u.chars
		}
		day += u.chars
	}
	switch {
	case hour > speakCharsPerHour:
		return "rate"
	case day > speakCharsPerDay:
		return "quota"
	}
	s.used[key] = append(s.used[key], speakUse{at: now, chars: chars})
	return ""
}

// refund gives back a use for a text the engine never got.
func (s *speakState) refund(login string, chars int, at time.Time) {
	key := strings.ToLower(login)
	s.mu.Lock()
	defer s.mu.Unlock()
	us := s.used[key]
	for i := len(us) - 1; i >= 0; i-- {
		if us[i].at.Equal(at) && us[i].chars == chars {
			s.used[key] = append(us[:i], us[i+1:]...)
			return
		}
	}
}

type speechInfo struct {
	Voice    string             `json:"voice"` // the tool's default
	Voices   []voice.SpeakVoice `json:"voices"`
	MaxChars int                `json:"maxChars"`
}

func (g *gate) speechInfoFor(login string) *speechInfo {
	if g.speak == nil || !g.readAloudFor(login) || g.speak.cfg.Available == nil {
		return nil
	}
	h, ok := g.speak.cfg.Available()
	if !ok || len(h.Voices) == 0 || h.MaxChars <= 0 {
		return nil
	}
	return &speechInfo{Voice: h.Voice, Voices: h.Voices, MaxChars: h.MaxChars}
}

func validMessageRef(s string) bool {
	if s == "" || len(s) > maxMessageRef {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-') {
			return false
		}
	}
	return true
}

func (g *gate) refuseSpeak(w http.ResponseWriter, r *http.Request, line *AuditLine, status int, word string) {
	line.Reason = word
	g.answerFileJSON(w, r, line, DecisionDenySpeak, status, map[string]any{"error": word})
}

func (g *gate) serveSpeak(w http.ResponseWriter, r *http.Request, line *AuditLine, v viewer) {
	s := g.speak
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		g.refuseSpeak(w, r, line, http.StatusMethodNotAllowed, "method")
		return
	}
	if s == nil || !g.readAloudFor(v.login) {
		g.refuseSpeak(w, r, line, http.StatusForbidden, "voice-off")
		return
	}
	if !sameOriginWrite(r, g.cfg.ServeHost) || r.Header.Get(voiceHeader) != "1" {
		g.refuseSpeak(w, r, line, http.StatusForbidden, "origin")
		return
	}
	if !s.tries.take(v.login, s.now()) {
		w.Header().Set("Retry-After", "600")
		g.refuseSpeak(w, r, line, http.StatusTooManyRequests, "rate")
		return
	}
	var health voice.SpeakHealth
	ok := false
	if s.cfg.Available != nil {
		health, ok = s.cfg.Available()
	}
	if !ok || s.cfg.Speak == nil {
		line.Error = "lever-tool-tts is not ready (see lever doctor and the tool log)"
		g.refuseSpeak(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		g.refuseSpeak(w, r, line, http.StatusUnsupportedMediaType, "bad-request")
		return
	}
	if r.ContentLength > speakBody {
		g.refuseSpeak(w, r, line, http.StatusRequestEntityTooLarge, "too-long")
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(speakBodyWait))
	var req struct {
		Agent string `json:"agent"`
		ID    string `json:"id"`
		Text  string `json:"text"`
		Voice string `json:"voice"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, speakBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	_ = rc.SetReadDeadline(time.Time{})
	switch {
	case tooLarge(err):
		g.refuseSpeak(w, r, line, http.StatusRequestEntityTooLarge, "too-long")
		return
	case timedOut(err):
		g.refuseSpeak(w, r, line, http.StatusRequestTimeout, "timeout")
		return
	case err != nil:
		g.refuseSpeak(w, r, line, http.StatusBadRequest, "bad-request")
		return
	}
	if !validMessageRef(req.ID) || req.Agent == "" {
		g.refuseSpeak(w, r, line, http.StatusBadRequest, "bad-request")
		return
	}
	line.MessageID = req.ID
	if !slices.Contains(v.message, req.Agent) && !slices.Contains(v.see, req.Agent) {
		// The name is not one of the login's agents: not written to the
		// audit line, which would otherwise carry any text a login chose.
		g.refuseSpeak(w, r, line, http.StatusForbidden, "agent")
		return
	}
	line.Agent = req.Agent
	if req.Voice != "" && !slices.ContainsFunc(health.Voices, func(x voice.SpeakVoice) bool { return x.Name == req.Voice }) {
		g.refuseSpeak(w, r, line, http.StatusBadRequest, "bad-voice")
		return
	}
	n := utf8.RuneCountInString(req.Text)
	line.Chars = n
	if n > health.MaxChars {
		g.refuseSpeak(w, r, line, http.StatusRequestEntityTooLarge, "too-long")
		return
	}
	text, err := voice.SpeechInput(req.Text, health.MaxChars)
	if err != nil {
		g.refuseSpeak(w, r, line, http.StatusBadRequest, "bad-text")
		return
	}
	takenAt := s.now()
	if word := s.take(v.login, n, takenAt); word != "" {
		w.Header().Set("Retry-After", "600")
		g.refuseSpeak(w, r, line, http.StatusTooManyRequests, word)
		return
	}
	done, ok := s.begin(v.login, v.tier == chatledger.TierOperator)
	if !ok {
		s.refund(v.login, n, takenAt)
		w.Header().Set("Retry-After", "5")
		g.refuseSpeak(w, r, line, http.StatusTooManyRequests, "busy")
		return
	}
	defer done()
	select {
	case s.run <- struct{}{}:
	case <-r.Context().Done():
		s.refund(v.login, n, takenAt)
		line.Error = "the request ended while it waited for its turn"
		g.refuseSpeak(w, r, line, http.StatusServiceUnavailable, "busy")
		return
	}
	start := time.Now()
	// A page that goes away (stop, or a hidden page) ends the request to
	// the tool, which kills the engine's run.
	wav, err := s.cfg.Speak(r.Context(), text, req.Voice)
	<-s.run
	line.LatencyMS = max(time.Since(start).Milliseconds(), 1)
	if err != nil {
		if s.cfg.NotSent != nil && s.cfg.NotSent(err) {
			s.refund(v.login, n, takenAt)
		}
		line.Error = "speech failed: " + err.Error()
		g.refuseSpeak(w, r, line, http.StatusServiceUnavailable, "unavailable")
		return
	}
	g.answerChat(w, line, DecisionSpeak, http.StatusOK, func() {
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Security-Policy", "sandbox")
	}, wav, r)
}
