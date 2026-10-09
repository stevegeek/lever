package remoteproxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// wavClip is a canonical clip of n samples (PCM s16le, 16 kHz, mono).
func wavClip(n int) []byte {
	b := make([]byte, wavHeaderLen+2*n)
	le := binary.LittleEndian
	copy(b[0:], "RIFF")
	le.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	le.PutUint32(b[16:], 16)
	le.PutUint16(b[20:], 1)
	le.PutUint16(b[22:], 1)
	le.PutUint32(b[24:], 16000)
	le.PutUint32(b[28:], 32000)
	le.PutUint16(b[32:], 2)
	le.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	le.PutUint32(b[40:], uint32(2*n))
	return b
}

// voiceRec is a transcriber that records what it was sent.
type voiceRec struct {
	mu    sync.Mutex
	clips [][]byte
	text  string
	err   error
	up    atomic.Bool
	block chan struct{} // when set, each call waits for one receive
	in    atomic.Int32  // calls running now
	most  atomic.Int32  // the most at once
}

func (v *voiceRec) transcribe(ctx context.Context, wav []byte) (string, error) {
	n := v.in.Add(1)
	defer v.in.Add(-1)
	for {
		m := v.most.Load()
		if n <= m || v.most.CompareAndSwap(m, n) {
			break
		}
	}
	if v.block != nil {
		select {
		case <-v.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.clips = append(v.clips, bytes.Clone(wav))
	return v.text, v.err
}

// voiceCfg: chatConfig (operator op@x, contact c@x) plus operator op2@x,
// contacts d@x and e@x, and voice on with max_seconds 2.
func voiceCfg(t *testing.T, hub *pageHub) (Config, *voiceRec) {
	cfg := chatConfig(t, hub)
	cfg.AllowedUsers = append(cfg.AllowedUsers, "op2@x", "d@x", "e@x", "nov@x")
	cfg.Contacts["d@x"] = []string{"w1"}
	cfg.Contacts["e@x"] = []string{"w1"}
	rec := &voiceRec{text: "  hello Lever \n"}
	rec.up.Store(true)
	cfg.Voice = &VoiceConfig{MaxSeconds: 2, Excluded: []string{"NOV@x"}, Available: rec.up.Load, Transcribe: rec.transcribe}
	return cfg, rec
}

func voicePost(h http.Handler, login string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	req := proxyRequest("POST", voicePath, bytes.NewReader(body))
	req.Header.Set("Tailscale-User-Login", login)
	req.Header.Set("Content-Type", "audio/wav")
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

func errorWord(rw *httptest.ResponseRecorder) string {
	var b struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &b)
	return b.Error
}

func TestVoiceRouteAbsentWhenOff(t *testing.T) {
	hub := newPageHub(t)
	h := NewHandler(chatConfig(t, hub))
	for _, m := range []string{"GET", "POST"} {
		for _, p := range []string{voicePath, "/lever/api/voice", "/lever/api/voice/x"} {
			if rw := chatDo(h, chatOp, m, p); rw.Code != http.StatusNotFound {
				t.Fatalf("%s %s off = %d", m, p, rw.Code)
			}
		}
	}
	cfg, _ := voiceCfg(t, hub)
	if rw := chatDo(NewHandler(cfg), chatOp, "POST", "/lever/api/voice/other"); rw.Code != http.StatusNotFound {
		t.Fatalf("another path under the prefix = %d", rw.Code)
	}
	if len(hub.reached()) != 0 {
		t.Fatalf("the hub was asked: %v", hub.reached())
	}
}

func TestVoiceTranscribes(t *testing.T) {
	hub := newPageHub(t)
	cfg, rec := voiceCfg(t, hub)
	var lines lockedLines
	cfg.Audit = lines.add
	clip := wavClip(24000) // 1.5 s
	rw := voicePost(NewHandler(cfg), "c@x", clip)
	if rw.Code != http.StatusOK || strings.TrimSpace(rw.Body.String()) != `{"text":"hello Lever"}` {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	for k, v := range map[string]string{"Content-Type": "application/json", "Content-Security-Policy": "sandbox", "Cache-Control": "no-store"} {
		if got := rw.Header().Get(k); got != v {
			t.Errorf("%s = %q", k, got)
		}
	}
	if len(rec.clips) != 1 || !bytes.Equal(rec.clips[0], clip) {
		t.Fatal("the transcriber did not get the clip as sent")
	}
	all := lines.all()
	last := all[len(all)-1]
	if last.Decision != DecisionVoice || last.Status != 200 || last.AudioSeconds != 1.5 || last.LatencyMS < 1 || last.TSLogin != "c@x" {
		t.Fatalf("audit %+v", last)
	}
	// Never the text in the audit line.
	b, _ := json.Marshal(all)
	if strings.Contains(string(b), "hello") {
		t.Fatalf("the transcript reached the audit log: %s", b)
	}
	if len(hub.reached()) != 0 {
		t.Fatalf("the hub was asked: %v", hub.reached())
	}
}

func TestVoiceGate(t *testing.T) {
	hub := newPageHub(t)
	cfg, rec := voiceCfg(t, hub)
	h := NewHandler(cfg)
	clip := wavClip(16000)
	for name, tc := range map[string]struct {
		login  string
		body   []byte
		hdr    []string
		status int
		word   string
	}{
		"no header":      {chatOp, clip, []string{voiceHeader, ""}, 403, "origin"},
		"header not 1":   {chatOp, clip, []string{voiceHeader, "true"}, 403, "origin"},
		"other origin":   {chatOp, clip, []string{"Origin", "https://evil.test"}, 403, ""},
		"no origin":      {chatOp, clip, []string{"Origin", ""}, 403, "origin"},
		"cross-site":     {chatOp, clip, []string{"Sec-Fetch-Site", "cross-site"}, 403, ""},
		"excluded login": {"nov@x", clip, nil, 403, "voice-off"},
		"not wav":        {chatOp, clip, []string{"Content-Type", "audio/webm"}, 415, "bad-audio"},
		"no type":        {chatOp, clip, []string{"Content-Type", ""}, 415, "bad-audio"},
		"too long":       {chatOp, wavClip(2*16000 + 2), nil, 413, "too-long"},
		"too short":      {chatOp, wavClip(10), nil, 400, "bad-audio"},
		"not a wav":      {chatOp, bytes.Repeat([]byte("x"), 4000), nil, 400, "bad-audio"},
		"unknown login":  {"z@x", clip, nil, 403, ""},
	} {
		t.Run(name, func(t *testing.T) {
			rw := voicePost(h, tc.login, tc.body, tc.hdr...)
			if rw.Code != tc.status || tc.word != "" && errorWord(rw) != tc.word {
				t.Fatalf("%d %s, want %d %s", rw.Code, rw.Body, tc.status, tc.word)
			}
		})
	}
	if rw := chatDo(h, chatOp, "GET", voicePath); rw.Code != http.StatusMethodNotAllowed || rw.Header().Get("Allow") != "POST" {
		t.Fatalf("GET = %d", rw.Code)
	}
	rec.up.Store(false)
	if rw := voicePost(h, chatOp, clip); rw.Code != http.StatusServiceUnavailable || errorWord(rw) != "unavailable" {
		t.Fatalf("down: %d %s", rw.Code, rw.Body)
	}
	if len(rec.clips) != 0 {
		t.Fatalf("a refused clip reached the transcriber: %d", len(rec.clips))
	}
}

func TestVoiceBodyLimitAndDeadline(t *testing.T) {
	s := newVoiceState(VoiceConfig{MaxSeconds: 300})
	if s.maxBody() != 32000*300+1024 {
		t.Fatal(s.maxBody())
	}
	if d := s.bodyDeadline(); d < 3*time.Minute || d > 4*time.Minute {
		t.Fatalf("deadline %v for 300 s", d)
	}
	if d := newVoiceState(VoiceConfig{MaxSeconds: 600}).bodyDeadline(); d > voiceBodyMax {
		t.Fatalf("deadline %v", d)
	}
	// A Content-Length over the limit is refused before the body is read.
	hub := newPageHub(t)
	cfg, rec := voiceCfg(t, hub)
	req := proxyRequest("POST", voicePath, strings.NewReader("x"))
	req.ContentLength = 32000*2 + 1025
	for k, v := range map[string]string{"Tailscale-User-Login": chatOp, "Content-Type": "audio/wav", "Origin": "https://" + testServeHost, voiceHeader: "1"} {
		req.Header.Set(k, v)
	}
	rw := httptest.NewRecorder()
	NewHandler(cfg).ServeHTTP(rw, req)
	if rw.Code != http.StatusRequestEntityTooLarge || len(rec.clips) != 0 {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
}

func TestCheckWAV(t *testing.T) {
	good := wavClip(16000)
	if sec, word := checkWAV(good, 1); sec != 1 || word != "" {
		t.Fatalf("%v %q", sec, word)
	}
	if _, word := checkWAV(wavClip(16001), 1); word != "too-long" {
		t.Fatalf("over max: %q", word)
	}
	le := binary.LittleEndian
	for name, mut := range map[string]func(b []byte) []byte{
		"riff":        func(b []byte) []byte { copy(b, "RIFX"); return b },
		"riff size":   func(b []byte) []byte { le.PutUint32(b[4:], 1); return b },
		"wave":        func(b []byte) []byte { copy(b[8:], "AVI "); return b },
		"fmt size":    func(b []byte) []byte { le.PutUint32(b[16:], 18); return b },
		"float":       func(b []byte) []byte { le.PutUint16(b[20:], 3); return b },
		"extensible":  func(b []byte) []byte { le.PutUint16(b[20:], 0xfffe); return b },
		"stereo":      func(b []byte) []byte { le.PutUint16(b[22:], 2); return b },
		"44.1 kHz":    func(b []byte) []byte { le.PutUint32(b[24:], 44100); return b },
		"byte rate":   func(b []byte) []byte { le.PutUint32(b[28:], 16000); return b },
		"block align": func(b []byte) []byte { le.PutUint16(b[32:], 4); return b },
		"8 bit":       func(b []byte) []byte { le.PutUint16(b[34:], 8); return b },
		"list chunk":  func(b []byte) []byte { copy(b[36:], "LIST"); return b },
		"data size":   func(b []byte) []byte { le.PutUint32(b[40:], uint32(len(b))); return b },
		"trailing":    func(b []byte) []byte { return append(b, 0, 0) },
		"odd data": func(b []byte) []byte {
			b = append(b, 0)
			le.PutUint32(b[4:], uint32(len(b)-8))
			le.PutUint32(b[40:], uint32(len(b)-44))
			return b
		},
		"header only":  func(b []byte) []byte { return wavClip(0) },
		"truncated":    func(b []byte) []byte { return b[:30] },
		"empty":        func(b []byte) []byte { return nil },
		"short clip":   func(b []byte) []byte { return wavClip(799) },
		"riff in data": func(b []byte) []byte { return append([]byte("RIFF"), b...) },
	} {
		if _, word := checkWAV(mut(bytes.Clone(good)), 1); word != "bad-audio" {
			t.Errorf("%s: %q", name, word)
		}
	}
	if _, word := checkWAV(wavClip(1600), 1); word != "" {
		t.Fatalf("a tenth of a second: %q", word)
	}
}

func TestVoiceSlots(t *testing.T) {
	s := newVoiceState(VoiceConfig{MaxSeconds: 1})
	d1, ok := s.begin("op@x", true)
	if !ok {
		t.Fatal("first")
	}
	if _, ok := s.begin("OP@x", true); ok {
		t.Fatal("a login holds one clip at a time")
	}
	d2, ok := s.begin("c@x", false)
	if !ok {
		t.Fatal("a contact takes the second of three")
	}
	if _, ok := s.begin("d@x", false); ok {
		t.Fatal("a contact took the last slot")
	}
	d3, ok := s.begin("op2@x", true)
	if !ok {
		t.Fatal("an operator takes the last slot")
	}
	if _, ok := s.begin("op3@x", true); ok {
		t.Fatal("more than three slots")
	}
	d1()
	d2()
	d3()
	if s.total != 0 || len(s.held) != 0 {
		t.Fatalf("slots not given back: %d %v", s.total, s.held)
	}
}

func TestVoiceLimits(t *testing.T) {
	s := newVoiceState(VoiceConfig{MaxSeconds: 600})
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	for i := range voiceClipsPerHour {
		if w := s.limit("op@x", 1, true); w != "" {
			t.Fatalf("clip %d: %s", i, w)
		}
	}
	if w := s.limit("OP@X", 1, true); w != "rate" {
		t.Fatalf("31st clip in an hour: %q", w)
	}
	if w := s.limit("c@x", 1, true); w != "" {
		t.Fatalf("another login: %q", w)
	}
	now = now.Add(time.Hour)
	if w := s.limit("op@x", 1, true); w != "" {
		t.Fatalf("an hour later: %q", w)
	}
	// Audio per day: 60 minutes.
	s2 := newVoiceState(VoiceConfig{MaxSeconds: 600})
	s2.now = s.now
	for range 6 {
		if w := s2.limit("op@x", 600, true); w != "" {
			t.Fatal(w)
		}
	}
	if w := s2.limit("op@x", 1, true); w != "quota" {
		t.Fatalf("over an hour of audio: %q", w)
	}
	if w := s2.limit("op@x", 0, false); w != "" {
		t.Fatalf("the peek at exactly the quota: %q", w)
	}
	now = now.Add(24 * time.Hour)
	if w := s2.limit("op@x", 600, true); w != "" {
		t.Fatalf("a day later: %q", w)
	}
}

func TestVoiceRateOverTheRoute(t *testing.T) {
	hub := newPageHub(t)
	cfg, _ := voiceCfg(t, hub)
	h := NewHandler(cfg)
	// Attempts (refused ones included) are bounded too.
	for i := range voiceTriesPerHour {
		if rw := voicePost(h, chatOp, []byte("junk")); rw.Code != http.StatusBadRequest {
			t.Fatalf("try %d: %d", i, rw.Code)
		}
	}
	if rw := voicePost(h, chatOp, wavClip(16000)); rw.Code != http.StatusTooManyRequests || errorWord(rw) != "rate" {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
}

func TestVoiceOneAtATimeOnTheGPU(t *testing.T) {
	hub := newPageHub(t)
	cfg, rec := voiceCfg(t, hub)
	rec.block = make(chan struct{})
	h := NewHandler(cfg)
	clip := wavClip(16000)
	results := make(chan int, 3)
	var wg sync.WaitGroup
	for _, login := range []string{chatOp, "c@x"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- voicePost(h, login, clip).Code
		}()
	}
	g := h.(*gate)
	deadline := time.Now().Add(5 * time.Second)
	for {
		g.voice.mu.Lock()
		n := g.voice.total
		g.voice.mu.Unlock()
		if n == 2 && rec.in.Load() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slots %d, running %d", n, rec.in.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Two slots held: a contact may not take the last one, an operator may.
	if rw := voicePost(h, "d@x", clip); rw.Code != http.StatusTooManyRequests || errorWord(rw) != "busy" {
		t.Fatalf("contact on the last slot: %d %s", rw.Code, rw.Body)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		results <- voicePost(h, "op2@x", clip).Code
	}()
	for range 3 {
		rec.block <- struct{}{}
	}
	wg.Wait()
	close(results)
	for code := range results {
		if code != 200 {
			t.Fatalf("a queued clip answered %d", code)
		}
	}
	if rec.most.Load() != 1 {
		t.Fatalf("%d transcriptions ran at once", rec.most.Load())
	}
}

func TestVoiceTranscriberFailure(t *testing.T) {
	hub := newPageHub(t)
	cfg, rec := voiceCfg(t, hub)
	rec.err = errors.New("whisper-server: HTTP 500")
	var lines lockedLines
	cfg.Audit = lines.add
	rw := voicePost(NewHandler(cfg), chatOp, wavClip(16000))
	if rw.Code != http.StatusServiceUnavailable || errorWord(rw) != "unavailable" || strings.Contains(rw.Body.String(), "500") {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	all := lines.all()
	if last := all[len(all)-1]; last.Decision != DecisionDenyVoice || !strings.Contains(last.Error, "HTTP 500") {
		t.Fatalf("%+v", last)
	}
}

func TestVoiceText(t *testing.T) {
	if got := voiceText(" \n hi there \t"); got != "hi there" {
		t.Fatalf("%q", got)
	}
	long := strings.Repeat("é", voiceMaxText+10)
	if got := voiceText(long); len([]rune(got)) != voiceMaxText {
		t.Fatalf("%d", len([]rune(got)))
	}
	if got := voiceText("a\xffb"); got != "a�b" {
		t.Fatalf("%q", got)
	}
}

func TestAgentsAnswerCarriesVoice(t *testing.T) {
	hub := newPageHub(t)
	cfg, rec := voiceCfg(t, hub)
	for login, want := range map[string]bool{chatOp: true, "c@x": true, "nov@x": false} {
		rw := chatDo(NewHandler(cfg), login, "GET", "/lever/api/agents")
		if got := strings.Contains(rw.Body.String(), `"voice":{"maxSeconds":2}`); got != want {
			t.Fatalf("%s: %v: %s", login, got, rw.Body)
		}
	}
	rec.up.Store(false)
	if rw := chatDo(NewHandler(cfg), chatOp, "GET", "/lever/api/agents"); strings.Contains(rw.Body.String(), `"voice"`) {
		t.Fatalf("transcriber down: %s", rw.Body)
	}
	if rw := chatDo(NewHandler(chatConfig(t, hub)), chatOp, "GET", "/lever/api/agents"); strings.Contains(rw.Body.String(), `"voice"`) {
		t.Fatalf("off: %s", rw.Body)
	}
}

// TestVoiceAnswersAreNotCached: a transcript is for the one request.
func TestVoiceAnswersAreNotCached(t *testing.T) {
	hub := newPageHub(t)
	cfg, _ := voiceCfg(t, hub)
	rw := voicePost(NewHandler(cfg), chatOp, wavClip(16000))
	if rw.Code != 200 || rw.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", rw.Code, rw.Header())
	}
}

// TestVoiceSlowTranscriptionOutlivesTheBodyDeadline: the body deadline is
// for the upload only. Left set, net/http's background read would cancel
// the request once it passed, while the clip waits or is transcribed.
func TestVoiceSlowTranscriptionOutlivesTheBodyDeadline(t *testing.T) {
	hub := newPageHub(t)
	cfg, rec := voiceCfg(t, hub)
	slow := rec.transcribe
	cfg.Voice.Transcribe = func(ctx context.Context, wav []byte) (string, error) {
		select {
		case <-time.After(400 * time.Millisecond):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return slow(ctx, wav)
	}
	h := NewHandler(cfg)
	h.(*gate).voice.deadline = 100 * time.Millisecond
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, err := http.NewRequest("POST", srv.URL+voicePath, bytes.NewReader(wavClip(16000)))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testServeHost
	for k, v := range map[string]string{"Tailscale-User-Login": chatOp, "Content-Type": "audio/wav", "Origin": "https://" + testServeHost,
		"Sec-Fetch-Site": "same-origin", voiceHeader: "1"} {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(body.String(), "hello Lever") {
		t.Fatalf("%d %s", resp.StatusCode, body.String())
	}
}
