package remoteproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/voice"
)

// wavClip is a canonical clip of n samples (PCM s16le, 16 kHz, mono).
func wavClip(n int) []byte { return voice.CanonicalWAV(n) }

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

// A clip that never reached the transcriber gives its use of the limits
// back; a transcription that ran and failed (a timeout, a server error)
// counts: otherwise over-long clips could dodge the daily minutes.
func TestVoiceFailureRefundsOnlyUnsentClips(t *testing.T) {
	hub := newPageHub(t)
	cfg, rec := voiceCfg(t, hub)
	notSent := errors.New("not sent")
	cfg.Voice.NotSent = func(err error) bool { return errors.Is(err, notSent) }
	h := NewHandler(cfg)
	g := h.(*gate)
	uses := func() int {
		g.voice.mu.Lock()
		defer g.voice.mu.Unlock()
		return len(g.voice.used[strings.ToLower(chatOp)])
	}
	rec.err = fmt.Errorf("dial: %w", notSent)
	for range 3 {
		if rw := voicePost(h, chatOp, wavClip(16000)); rw.Code != http.StatusServiceUnavailable {
			t.Fatalf("%d %s", rw.Code, rw.Body)
		}
	}
	if n := uses(); n != 0 {
		t.Fatalf("%d uses after unsent clips, want 0", n)
	}
	rec.err = errors.New("context deadline exceeded")
	if rw := voicePost(h, chatOp, wavClip(16000)); rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d %s", rw.Code, rw.Body)
	}
	if n := uses(); n != 1 {
		t.Fatalf("%d uses after a timed-out transcription, want 1", n)
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

// readAloud rides the roster for logins with voice on, transcriber or not,
// unless read_aloud is off; never with voice off.
func TestAgentsAnswerCarriesReadAloud(t *testing.T) {
	hub := newPageHub(t)
	cfg, rec := voiceCfg(t, hub)
	cfg.Voice.ReadAloud = true
	rec.up.Store(false)
	for login, want := range map[string]bool{chatOp: true, "c@x": true, "nov@x": false} {
		rw := chatDo(NewHandler(cfg), login, "GET", "/lever/api/agents")
		if got := strings.Contains(rw.Body.String(), `"readAloud":true`); got != want {
			t.Fatalf("%s: %v: %s", login, got, rw.Body)
		}
	}
	cfg.Voice.ReadAloud = false
	if rw := chatDo(NewHandler(cfg), chatOp, "GET", "/lever/api/agents"); strings.Contains(rw.Body.String(), `readAloud`) {
		t.Fatalf("read_aloud off: %s", rw.Body)
	}
	if rw := chatDo(NewHandler(chatConfig(t, hub)), chatOp, "GET", "/lever/api/agents"); strings.Contains(rw.Body.String(), `readAloud`) {
		t.Fatalf("voice off: %s", rw.Body)
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

// fakeTool is a stand-in for lever-tool-whisper's dictation socket: health
// answers ready, and each clip gets the answer set in word ("" = a
// transcript).
type fakeTool struct {
	mu    sync.Mutex
	word  string
	clips int
}

func (f *fakeTool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/health":
		_ = json.NewEncoder(w).Encode(voice.Health{Ready: true, Model: "m", MaxSeconds: 2})
	case "/dictate":
		f.mu.Lock()
		word := f.word
		f.clips++
		f.mu.Unlock()
		if word == "" {
			_ = json.NewEncoder(w).Encode(map[string]string{"text": " hi [BLANK_AUDIO] there "})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": word})
	}
}

// The proxy over the real socket client, against a fake tool: the text is
// cleaned, and only a clip that never reached whisper-server (the tool
// answers not-ready, or the socket is gone) gives its use back.
func TestVoiceThroughTheDictationSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "rp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s", "d.sock")
	ln, err := voice.ListenDictate(sock)
	if err != nil {
		t.Fatal(err)
	}
	tool := &fakeTool{}
	srv := &http.Server{Handler: tool}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	hub := newPageHub(t)
	cfg, _ := voiceCfg(t, hub)
	client := &voice.DictateClient{Socket: sock, HealthTTL: time.Millisecond}
	cfg.Voice.Available, cfg.Voice.Transcribe = client.Available, client.Transcribe
	cfg.Voice.NotSent = func(err error) bool { return errors.Is(err, voice.ErrNotSent) }
	h := NewHandler(cfg)
	g := h.(*gate)
	uses := func() int {
		g.voice.mu.Lock()
		defer g.voice.mu.Unlock()
		return len(g.voice.used[strings.ToLower(chatOp)])
	}
	if rw := chatDo(h, chatOp, "GET", "/lever/api/agents"); !strings.Contains(rw.Body.String(), `"voice":{"maxSeconds":2}`) {
		t.Fatalf("roster: %s", rw.Body)
	}
	rw := voicePost(h, chatOp, wavClip(16000))
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), `"text":"hi there"`) || uses() != 1 {
		t.Fatalf("%d %s %d", rw.Code, rw.Body, uses())
	}
	tool.word = voice.WordNotReady
	if rw := voicePost(h, chatOp, wavClip(16000)); rw.Code != http.StatusServiceUnavailable || uses() != 1 {
		t.Fatalf("not ready: %d %s, %d uses", rw.Code, rw.Body, uses())
	}
	tool.word = voice.WordFailed
	if rw := voicePost(h, chatOp, wavClip(16000)); rw.Code != http.StatusServiceUnavailable || uses() != 2 {
		t.Fatalf("failed: %d %s, %d uses", rw.Code, rw.Body, uses())
	}
	// The tool goes away between the health check and the clip: given back.
	tool.word = ""
	h2 := &voice.DictateClient{Socket: sock, HealthTTL: time.Hour}
	if !h2.Available() {
		t.Fatal("not available")
	}
	cfg.Voice.Available, cfg.Voice.Transcribe = h2.Available, h2.Transcribe
	h = NewHandler(cfg)
	g = h.(*gate)
	srv.Close()
	if rw := voicePost(h, chatOp, wavClip(16000)); rw.Code != http.StatusServiceUnavailable || uses() != 0 {
		t.Fatalf("socket gone: %d %s, %d uses", rw.Code, rw.Body, uses())
	}
	time.Sleep(5 * time.Millisecond) // past client's HealthTTL
	if client.Available() {
		t.Fatal("available with the socket gone")
	}
}

// slowSvc is a ready transcriber for the real dictation handler that
// reports each start and works until its context ends.
type slowSvc struct{ started chan struct{} }

func (s *slowSvc) Ready() bool { return true }
func (s *slowSvc) Transcribe(ctx context.Context, _ []byte) (string, error) {
	s.started <- struct{}{}
	<-ctx.Done()
	return "", ctx.Err()
}

// A browser that gives up while its clip waits in the tool's queue (behind
// an agent clip) gets its use back; one that gives up after the clip's
// transcription started does not.
func TestVoiceRefundWhenCanceledInTheToolQueue(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "rp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s", "d.sock")
	ln, err := voice.ListenDictate(sock)
	if err != nil {
		t.Fatal(err)
	}
	svc := &slowSvc{started: make(chan struct{}, 1)}
	queue := &voice.Queue{}
	srv := voice.NewDictateServer(&voice.DictateHandler{Svc: svc, Queue: queue, MaxSeconds: 2})
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	hub := newPageHub(t)
	cfg, _ := voiceCfg(t, hub)
	client := &voice.DictateClient{Socket: sock}
	cfg.Voice.Transcribe = client.Transcribe
	cfg.Voice.NotSent = func(err error) bool { return errors.Is(err, voice.ErrNotSent) }
	h := NewHandler(cfg)
	g := h.(*gate)
	uses := func() int {
		g.voice.mu.Lock()
		defer g.voice.mu.Unlock()
		return len(g.voice.used[strings.ToLower(chatOp)])
	}
	post := func(ctx context.Context) *httptest.ResponseRecorder {
		req := proxyRequest("POST", voicePath, bytes.NewReader(wavClip(16000))).WithContext(ctx)
		req.Header.Set("Tailscale-User-Login", chatOp)
		req.Header.Set("Content-Type", "audio/wav")
		req.Header.Set("Origin", "https://"+testServeHost)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set(voiceHeader, "1")
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		return rw
	}

	// An agent clip holds the tool's GPU; the browser gives up meanwhile.
	if err := queue.Acquire(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	rw := post(ctx)
	cancel()
	queue.Release()
	if rw.Code != http.StatusServiceUnavailable || uses() != 0 {
		t.Fatalf("canceled while queued: %d %s, %d uses", rw.Code, rw.Body, uses())
	}
	select {
	case <-svc.started:
		t.Fatal("the clip reached the transcriber")
	default:
	}

	// The transcription starts, then the browser gives up: counted.
	ctx, cancel = context.WithCancel(context.Background())
	go func() {
		<-svc.started
		time.Sleep(100 * time.Millisecond) // the tool's 102 reaches the proxy
		cancel()
	}()
	rw = post(ctx)
	cancel()
	if rw.Code != http.StatusServiceUnavailable || uses() != 1 {
		t.Fatalf("canceled during transcription: %d %s, %d uses", rw.Code, rw.Body, uses())
	}
}
