package remoteproxy

// Read-aloud with a server voice (remote.voice.read_aloud: server):
// POST /lever/api/voice/speak takes one part of an agent message the page
// shows and answers it spoken, as a WAV, by the broker tool lever-tool-tts
// on the operator's own host (over its speech socket: package voice).
// Nothing is stored: the text is read into memory, cleaned, handed to the
// tool and dropped; the audio goes back only to the login that asked. The
// audit line has the login, the message reference, the length, the outcome
// and the latency; never the text, never the audio.
//
// The route takes the text from the page, not only a message reference: a
// server-side lookup would have to repeat, exactly, every rule of what a
// login is shown (a contact's filtered history above all), and a mistake
// there would read out a message the login may not see. Text the page
// sends is text the login already has. Any text a login sends is spoken,
// so the route is a speech service for the logins, but a bounded one: the
// text is bounded per request (the tool's -max-chars) and per login
// (characters an hour and a day), the reference must name an agent the
// login's roster lists, and voice: false on a login turns it off.

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
	// Available is the tool's health answer when it answers ready
	// (voice.SpeakClient, cached briefly). False: the roster carries no
	// server voice, the page reads with the device's voices, and the route
	// answers unavailable.
	Available func() (voice.SpeakHealth, bool)
	// Speak turns one text into a WAV with voice ("" = the tool's
	// default). Its error goes to the audit line, so it must never carry
	// the text.
	Speak func(ctx context.Context, text, voice string) ([]byte, error)
	// NotSent reports whether a Speak error means the text never reached
	// the engine (voice.ErrSpeakNotSent): only then is its use of the
	// limits given back. nil = never.
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
	// One text with the engine at a time, and two waiting: speakSlots in
	// all, of which a contact never takes the last, so the operator can
	// always listen. A login has one request in flight at most (the page
	// asks for one part while the one before plays).
	speakSlots    = 3
	speakPerLogin = 1
	// Per login, in memory: characters spoken an hour and a day, and
	// requests (refused ones included) an hour.
	speakCharsPerHour = 40000
	speakCharsPerDay  = 200000
	speakTriesPerHour = 1200
	// speakBodyWait bounds reading the request body.
	speakBodyWait = 30 * time.Second
	// maxMessageRef bounds the message reference.
	maxMessageRef = 128
)

// speakState holds the route's slots and per-login counts.
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

// begin takes a slot for login, or reports false.
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

// take counts chars for login at now, or answers the refusal word ("rate":
// over the hour's characters, "quota": over the day's). Uses older than a
// day are forgotten.
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

// refund gives back a use take recorded at the same moment for the same
// length, for a text the engine never got.
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

// speechInfo is the roster's server voice entry: present only when the
// login has read-aloud, read_aloud is server and the tool answers ready.
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

// validMessageRef is a hub message id as the page has it: 1 to
// maxMessageRef characters of letters, digits and ._:-.
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

// refuseSpeak answers the route with one fixed error word.
func (g *gate) refuseSpeak(w http.ResponseWriter, r *http.Request, line *AuditLine, status int, word string) {
	line.Reason = word
	g.answerFileJSON(w, r, line, DecisionDenySpeak, status, map[string]any{"error": word})
}

// serveSpeak answers POST /lever/api/voice/speak for v.
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
