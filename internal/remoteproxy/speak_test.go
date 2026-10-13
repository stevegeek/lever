package remoteproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/voice"
)

var errNotSentTest = errors.New("not sent")

type speakRec struct {
	mu    sync.Mutex
	texts []string
	err   error
	up    atomic.Bool
	block chan struct{}
}

func (s *speakRec) available() (voice.SpeakHealth, bool) {
	if !s.up.Load() {
		return voice.SpeakHealth{}, false
	}
	return voice.SpeakHealth{Ready: true, Model: "kokoro-en-v0_19", Voice: "bf_emma",
		Voices: []voice.SpeakVoice{{Name: "bf_emma", Lang: "en-GB"}, {Name: "am_adam", Lang: "en-US"}}, MaxChars: 100}, true
}

func (s *speakRec) speak(ctx context.Context, text, v string) ([]byte, error) {
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.texts = append(s.texts, v+"|"+text)
	if s.err != nil {
		return nil, s.err
	}
	return []byte("RIFF-audio"), nil
}

// speakCfg is voiceCfg with read-aloud through a recording speech tool.
func speakCfg(t *testing.T, hub *pageHub) (Config, *speakRec) {
	cfg, _ := voiceCfg(t, hub)
	rec := &speakRec{}
	rec.up.Store(true)
	cfg.Voice.ReadAloud = true
	cfg.Voice.Speech = &SpeechConfig{Available: rec.available, Speak: rec.speak, NotSent: func(err error) bool { return errors.Is(err, errNotSentTest) }}
	return cfg, rec
}

func speakJSON(agent, id, text, v string) []byte {
	b, _ := json.Marshal(map[string]string{"agent": agent, "id": id, "text": text, "voice": v})
	return b
}

func speakPost(h http.Handler, login string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	req := proxyRequest("POST", speakPath, bytes.NewReader(body))
	req.Header.Set("Tailscale-User-Login", login)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://"+testServeHost)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set(voiceHeader, "1")
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			req.Header.Del(hdr[i])
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	return rw
}

func TestSpeakRouteAbsentWithoutSpeech(t *testing.T) {
	hub := newPageHub(t)
	cfg, _ := voiceCfg(t, hub)
	h := NewHandler(cfg)
	if rw := speakPost(h, chatOp, speakJSON("boss", "m1", "Hello.", "")); rw.Code != http.StatusNotFound {
		t.Fatalf("no speech = %d", rw.Code)
	}
	if rw := speakPost(NewHandler(chatConfig(t, hub)), chatOp, speakJSON("boss", "m1", "Hello.", "")); rw.Code != http.StatusNotFound {
		t.Fatalf("voice off = %d", rw.Code)
	}
	if len(hub.reached()) != 0 {
		t.Fatalf("the hub was asked: %v", hub.reached())
	}
}

func TestSpeakSpeaks(t *testing.T) {
	hub := newPageHub(t)
	cfg, rec := speakCfg(t, hub)
	var lines lockedLines
	cfg.Audit = lines.add
	rw := speakPost(NewHandler(cfg), "c@x", speakJSON("w1", "msg-1", " Hello\nthere, secret words. ", "am_adam"))
	if rw.Code != http.StatusOK || rw.Body.String() != "RIFF-audio" {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	for k, v := range map[string]string{"Content-Type": "audio/wav", "Content-Security-Policy": "sandbox", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff"} {
		if got := rw.Header().Get(k); got != v {
			t.Errorf("%s = %q", k, got)
		}
	}
	if len(rec.texts) != 1 || rec.texts[0] != "am_adam|Hello there, secret words." {
		t.Fatalf("the tool got %q", rec.texts)
	}
	all := lines.all()
	last := all[len(all)-1]
	if last.Decision != DecisionSpeak || last.Status != 200 || last.Agent != "w1" || last.MessageID != "msg-1" || last.Chars != 28 || last.LatencyMS < 1 || last.TSLogin != "c@x" {
		t.Fatalf("audit %+v", last)
	}
	b, _ := json.Marshal(all)
	if strings.Contains(string(b), "secret") {
		t.Fatal("the text reached the audit log")
	}
	// The default voice, and an agent the contact may only see.
	if rw := speakPost(NewHandler(cfg), "c@x", speakJSON("w2", "msg-2", "Hi.", "")); rw.Code != http.StatusOK {
		t.Fatalf("seen agent: %d %s", rw.Code, rw.Body)
	}
	if rec.texts[1] != "|Hi." {
		t.Fatalf("%q", rec.texts)
	}
}

func TestSpeakRefusals(t *testing.T) {
	hub := newPageHub(t)
	for name, tc := range map[string]struct {
		login  string
		body   []byte
		hdr    []string
		mut    func(c *Config, r *speakRec)
		status int
		word   string
	}{
		"get":             {chatOp, nil, nil, nil, http.StatusMethodNotAllowed, "method"},
		"voice off login": {"nov@x", speakJSON("boss", "m", "Hi.", ""), nil, nil, http.StatusForbidden, "voice-off"},
		"read aloud off":  {chatOp, speakJSON("boss", "m", "Hi.", ""), nil, func(c *Config, _ *speakRec) { c.Voice.ReadAloud = false }, http.StatusForbidden, "voice-off"},
		"no header":       {chatOp, speakJSON("boss", "m", "Hi.", ""), []string{voiceHeader, ""}, nil, http.StatusForbidden, "origin"},
		"cross origin":    {chatOp, speakJSON("boss", "m", "Hi.", ""), []string{"Origin", "https://evil.example"}, nil, http.StatusForbidden, ""},
		"cross site":      {chatOp, speakJSON("boss", "m", "Hi.", ""), []string{"Sec-Fetch-Site", "cross-site"}, nil, http.StatusForbidden, ""},
		"tool down":       {chatOp, speakJSON("boss", "m", "Hi.", ""), nil, func(_ *Config, r *speakRec) { r.up.Store(false) }, http.StatusServiceUnavailable, "unavailable"},
		"form":            {chatOp, speakJSON("boss", "m", "Hi.", ""), []string{"Content-Type", "text/plain"}, nil, http.StatusUnsupportedMediaType, "bad-request"},
		"not json":        {chatOp, []byte("Hi."), nil, nil, http.StatusBadRequest, "bad-request"},
		"extra field":     {chatOp, []byte(`{"agent":"boss","id":"m","text":"Hi.","ssml":"<x/>"}`), nil, nil, http.StatusBadRequest, "bad-request"},
		"no id":           {chatOp, speakJSON("boss", "", "Hi.", ""), nil, nil, http.StatusBadRequest, "bad-request"},
		"odd id":          {chatOp, speakJSON("boss", "m 1/2", "Hi.", ""), nil, nil, http.StatusBadRequest, "bad-request"},
		"other agent":     {"c@x", speakJSON("boss", "m", "Hi.", ""), nil, nil, http.StatusForbidden, "agent"},
		"unknown agent":   {chatOp, speakJSON("nobody", "m", "Hi.", ""), nil, nil, http.StatusForbidden, "agent"},
		"bad voice":       {chatOp, speakJSON("boss", "m", "Hi.", "zz"), nil, nil, http.StatusBadRequest, "bad-voice"},
		"too long":        {chatOp, speakJSON("boss", "m", strings.Repeat("a", 101), ""), nil, nil, http.StatusRequestEntityTooLarge, "too-long"},
		"huge body":       {chatOp, speakJSON("boss", "m", strings.Repeat("a", 70000), ""), nil, nil, http.StatusRequestEntityTooLarge, "too-long"},
		"nothing to say":  {chatOp, speakJSON("boss", "m", " \n--- ", ""), nil, nil, http.StatusBadRequest, "bad-text"},
		"tool fails":      {chatOp, speakJSON("boss", "m", "Hi.", ""), nil, func(_ *Config, r *speakRec) { r.err = errors.New("boom") }, http.StatusServiceUnavailable, "unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, rec := speakCfg(t, hub)
			if tc.mut != nil {
				tc.mut(&cfg, rec)
			}
			var lines lockedLines
			cfg.Audit = lines.add
			var rw *httptest.ResponseRecorder
			if tc.body == nil {
				rw = chatDo(NewHandler(cfg), tc.login, "GET", speakPath)
			} else {
				rw = speakPost(NewHandler(cfg), tc.login, tc.body, tc.hdr...)
			}
			if rw.Code != tc.status || errorWord(rw) != tc.word {
				t.Fatalf("%d %s, want %d %s", rw.Code, rw.Body, tc.status, tc.word)
			}
			if tc.word == "agent" {
				all := lines.all()
				if a := all[len(all)-1].Agent; a != "" {
					t.Fatalf("a refused agent name reached the audit line: %q", a)
				}
			}
			if tc.word != "unavailable" && len(rec.texts) != 0 {
				t.Fatalf("the tool was asked: %q", rec.texts)
			}
		})
	}
}

// Characters count per login an hour and a day; a text the engine never
// got is given back.
func TestSpeakLimits(t *testing.T) {
	hub := newPageHub(t)
	cfg, rec := speakCfg(t, hub)
	h := NewHandler(cfg)
	g := h.(*gate)
	now := time.Unix(1_800_000_000, 0)
	g.speak.now = func() time.Time { return now }
	text := strings.Repeat("a", 100)
	n := speakCharsPerHour / 100
	for i := range n {
		if rw := speakPost(h, chatOp, speakJSON("boss", "m", text, "")); rw.Code != http.StatusOK {
			t.Fatalf("%d: %d %s", i, rw.Code, rw.Body)
		}
	}
	if rw := speakPost(h, chatOp, speakJSON("boss", "m", text, "")); rw.Code != http.StatusTooManyRequests || errorWord(rw) != "rate" {
		t.Fatalf("over the hour: %d %s", rw.Code, rw.Body)
	}
	// Another login is not affected.
	if rw := speakPost(h, "op2@x", speakJSON("boss", "m", text, "")); rw.Code != http.StatusOK {
		t.Fatalf("other login: %d", rw.Code)
	}
	// An hour later the hour's room is back, until the day's runs out.
	for range speakCharsPerDay/speakCharsPerHour - 1 {
		now = now.Add(time.Hour)
		for range n {
			if rw := speakPost(h, chatOp, speakJSON("boss", "m", text, "")); rw.Code != http.StatusOK {
				t.Fatalf("%d %s", rw.Code, rw.Body)
			}
		}
	}
	now = now.Add(time.Hour)
	if rw := speakPost(h, chatOp, speakJSON("boss", "m", text, "")); rw.Code != http.StatusTooManyRequests || errorWord(rw) != "quota" {
		t.Fatalf("over the day: %d %s", rw.Code, rw.Body)
	}
	// Not sent: given back.
	cfg2, rec2 := speakCfg(t, hub)
	h2 := NewHandler(cfg2)
	rec2.err = errNotSentTest
	for range n + 5 {
		if rw := speakPost(h2, chatOp, speakJSON("boss", "m", text, "")); errorWord(rw) != "unavailable" {
			t.Fatalf("not sent: %d %s", rw.Code, rw.Body)
		}
	}
	rec2.err = nil
	if rw := speakPost(h2, chatOp, speakJSON("boss", "m", text, "")); rw.Code != http.StatusOK {
		t.Fatalf("after refunds: %d %s", rw.Code, rw.Body)
	}
	_ = rec
}

// A contact never takes the last slot, and a login has one request in
// flight.
func TestSpeakSlots(t *testing.T) {
	hub := newPageHub(t)
	cfg, rec := speakCfg(t, hub)
	rec.block = make(chan struct{})
	h := NewHandler(cfg)
	g := h.(*gate)
	var wg sync.WaitGroup
	codes := make(chan int, 4)
	post := func(login string) {
		defer wg.Done()
		codes <- speakPost(h, login, speakJSON("w1", "m", "Hi.", "")).Code
	}
	wg.Add(2)
	go post("c@x")
	go post("d@x")
	waitFor(t, 5*time.Second, func() bool {
		g.speak.mu.Lock()
		defer g.speak.mu.Unlock()
		return g.speak.total == 2
	})
	if rw := speakPost(h, "e@x", speakJSON("w1", "m", "Hi.", "")); rw.Code != http.StatusTooManyRequests || errorWord(rw) != "busy" {
		t.Fatalf("contact took the last slot: %d %s", rw.Code, rw.Body)
	}
	if rw := speakPost(h, "c@x", speakJSON("w1", "m", "Hi.", "")); rw.Code != http.StatusTooManyRequests {
		t.Fatalf("second request of a login: %d", rw.Code)
	}
	wg.Add(1)
	go post(chatOp)
	waitFor(t, 5*time.Second, func() bool {
		g.speak.mu.Lock()
		defer g.speak.mu.Unlock()
		return g.speak.total == 3
	})
	close(rec.block)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Fatalf("a queued request ended %d", c)
		}
	}
}

// The roster carries the server voices while the tool is ready, for logins
// with read-aloud.
func TestAgentsAnswerCarriesSpeech(t *testing.T) {
	hub := newPageHub(t)
	cfg, rec := speakCfg(t, hub)
	for login, want := range map[string]bool{chatOp: true, "c@x": true, "nov@x": false} {
		rw := chatDo(NewHandler(cfg), login, "GET", "/lever/api/agents")
		var ans struct {
			Speech *speechInfo `json:"speech"`
		}
		_ = json.Unmarshal(rw.Body.Bytes(), &ans)
		if (ans.Speech != nil) != want {
			t.Fatalf("%s: %s", login, rw.Body)
		}
		if want && (ans.Speech.Voice != "bf_emma" || len(ans.Speech.Voices) != 2 || ans.Speech.MaxChars != 100) {
			t.Fatalf("%s: %+v", login, ans.Speech)
		}
	}
	rec.up.Store(false)
	if rw := chatDo(NewHandler(cfg), chatOp, "GET", "/lever/api/agents"); strings.Contains(rw.Body.String(), `"speech"`) || !strings.Contains(rw.Body.String(), `"readAloud":true`) {
		t.Fatalf("tool down: %s", rw.Body)
	}
	cfg.Voice.ReadAloud = false
	rec.up.Store(true)
	if rw := chatDo(NewHandler(cfg), chatOp, "GET", "/lever/api/agents"); strings.Contains(rw.Body.String(), `"speech"`) {
		t.Fatalf("read aloud off: %s", rw.Body)
	}
}
