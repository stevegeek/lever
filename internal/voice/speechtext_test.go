package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLexicon() Lexicon {
	lex := Lexicon{}
	for _, w := range strings.Fields("hello there the agents opened on git hub and returned config i updated tests pass " +
		"don't exception zero one two three four five six seven eight nine ten eleven twelve thirteen twenty forty " +
		"fifty hundred thousand million billion point percent plus at first second third fourth " +
		"cafe uber minus dollar dollars pound pounds euro euros") {
		lex[w] = struct{}{}
	}
	for c := 'a'; c <= 'z'; c++ {
		lex[string(c)] = struct{}{}
	}
	return lex
}

func TestSpeakableText(t *testing.T) {
	lex := testLexicon()
	for in, want := range map[string]string{
		"Hello there.":                       "Hello there.",
		"The agents opened 3 PRs on GitHub.": "The agents opened three p r s on Git Hub.",
		"HTTP 503":                           "h t t p five hundred three",
		"1,000 and 3.5%":                     "one thousand and three point five percent",
		"v1.0.2":                             "v one point zero point two",
		"hello/there-hello_there":            "hello there hello there",
		"1st 22nd 3rd 4th":                   "first twenty second third fourth",
		"007":                                "zero zero seven",
		"gofmt":                              "g o f m t",
		"configs":                            "config",
		"don't":                              "don't",
		"don’t":                              "don't",
		"IOException":                        "i o Exception",
		"0":                                  "zero",
		"12 + 1 @ 3":                         "twelve plus one at three",
		"1,23":                               "one, twenty three",
		"(hello) \"there\"":                  "hello there",
		"123456789012":                       "one hundred twenty three billion four hundred fifty six million seven hundred eighty nine thousand twelve",
		"1234567890123":                      "one two three four five six seven eight nine zero one two three",
		// Accents are folded before lookup and spelling.
		"café":        "cafe",
		"cafe\u0301":  "cafe",
		"Straße über": "s t r a s s e uber",
		"ÉCOLE":       "e c o l e",
		// A sign or a currency before a number.
		"-5":    "minus five",
		"(−3)":  "minus three",
		"3-5":   "three five",
		"$5.99": "five point nine nine dollars",
		"£1":    "one pound",
		"€20":   "twenty euros",
	} {
		if got := SpeakableText(in, lex); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
	// Another script, or emoji only: nothing the lexicons hold is left.
	for _, in := range []string{"привет мир", "日本語", "😀 🎉"} {
		if got := SpeakableText(in, lex); got != "" {
			t.Errorf("%q: got %q, want nothing", in, got)
		}
	}
	for _, in := range []string{"!!! ...", "/ - _", "été"} {
		if got := SpeakableText(in, Lexicon{}); got != "" {
			t.Errorf("%q: got %q, want nothing", in, got)
		}
	}
	// Every word of the result is in the lexicon (or is punctuation on one).
	for _, in := range []string{"The agents opened 3 PRs on GitHub.", "1,000 and 3.5%", "Kubernetes v1.27 xyzzy"} {
		for _, w := range strings.Fields(SpeakableText(in, lex)) {
			if !lex.has(strings.TrimRight(w, ".,!?;:")) {
				t.Errorf("%q: %q is not in the lexicon", in, w)
			}
		}
	}
}

func TestLoadLexicon(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")
	if err := os.WriteFile(a, []byte("# Lexicon: word(s) || phones\nGitHub || g ɪ t\nnew york || n uː\n\nhello || h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("kokoro k ˈ O\nmisaki m i\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lex, err := LoadLexicon(a, b)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"github", "GitHub", "hello", "kokoro", "misaki"} {
		if !lex.has(w) {
			t.Errorf("%s missing", w)
		}
	}
	for _, w := range []string{"new", "new york", "#", "lexicon:"} {
		if lex.has(w) {
			t.Errorf("%q is in it", w)
		}
	}
	if _, err := LoadLexicon(filepath.Join(dir, "none.txt")); err == nil {
		t.Fatal("a missing lexicon was read")
	}
}

// A sherpa-onnx that cannot speak with the model fails the probe: the
// speaker is not ready and says what lever needs.
func TestSpeakerProbesTheProgram(t *testing.T) {
	s, l := testSpeaker(t)
	bin := filepath.Join(t.TempDir(), "old")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(fakeSherpa(t))
	if err != nil {
		t.Fatal(err)
	}
	s.Server = filepath.Join(bin, "sherpa-onnx-offline-tts")
	if err := os.WriteFile(s.Server, b, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "v1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Check(); err == nil || !strings.Contains(err.Error(), "sherpa-onnx 2.0 or later") {
		t.Fatalf("an old program: %v", err)
	}
	if s.Ready() {
		t.Fatal("ready with a program that cannot speak")
	}
	if _, err := s.Speak(context.Background(), "hello", ""); !errors.Is(err, ErrSpeakNotSent) {
		t.Fatalf("speak: %v", err)
	}
	if l.count("sherpa-onnx 2.0 or later") == 0 {
		t.Fatalf("not logged: %v", l.lines)
	}
	noSpeakTemp(t, s.ModelDir)
}

// The engine gets the rewritten text: numbers as words, every word known.
func TestSpeakerRewritesTheText(t *testing.T) {
	s, _ := testSpeaker(t)
	wav, err := s.Speak(context.Background(), "ARGV 25% now", "")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimRight(string(wav[WAVHeaderLen:]), "\x00"), "\x00")
	if got := args[len(args)-1]; got != "ARGV twenty five percent now" {
		t.Fatalf("the engine got %q", got)
	}
	if _, err := s.Speak(context.Background(), "!!! ---", ""); !errors.Is(err, ErrBadText) {
		t.Fatalf("nothing to speak: %v", err)
	}
}

// A failed probe stands only for probeRetryAfter; a program replaced in
// place is probed again at once.
func TestSpeakerProbeIsRetried(t *testing.T) {
	s, _ := testSpeaker(t)
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(fakeSherpa(t))
	if err != nil {
		t.Fatal(err)
	}
	s.Server = filepath.Join(bin, "sherpa-onnx-offline-tts")
	if err := os.WriteFile(s.Server, b, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(bin, "v1")
	if err := os.WriteFile(old, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if s.Ready() {
		t.Fatal("ready with a program that cannot speak")
	}
	// Fixed, but the failure still stands within probeRetryAfter.
	if err := os.Remove(old); err != nil {
		t.Fatal(err)
	}
	if s.Ready() {
		t.Fatal("a failed probe was run again at once")
	}
	saved := probeRetryAfter
	probeRetryAfter = 0
	defer func() { probeRetryAfter = saved }()
	if !s.Ready() {
		_, err := s.Check()
		t.Fatalf("not ready after the retry: %v", err)
	}
	// A program upgraded in place (same path, new file) is probed again,
	// whatever the passed probe said.
	probeRetryAfter = time.Hour
	if err := os.WriteFile(old, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(s.Server, future, future); err != nil {
		t.Fatal(err)
	}
	if s.Ready() {
		t.Fatal("a program replaced in place was not probed again")
	}
}

// Ready never waits for a run in progress: with the engine slot taken and
// no probe yet, it answers not ready at once.
func TestSpeakerReadyDoesNotWaitForARun(t *testing.T) {
	s, _ := testSpeaker(t)
	s.once.Do(func() { s.run = make(chan struct{}, 1) })
	s.run <- struct{}{}
	done := make(chan bool, 1)
	go func() { done <- s.Ready() }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("ready before any probe ran")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ready waited for the run in progress")
	}
	<-s.run
	if !s.Ready() {
		t.Fatal("not ready once the slot was free")
	}
}

// Text whose numbers grow it past four times the request limit is refused
// before the engine runs.
func TestSpeakerBoundsTheRewrittenText(t *testing.T) {
	s, _ := testSpeaker(t)
	long := strings.TrimSpace(strings.Repeat("777777777777 ", 15)) // 194 characters, under MaxChars 200
	_, err := s.Speak(context.Background(), long, "")
	if !errors.Is(err, ErrBadText) || !errors.Is(err, ErrSpeakNotSent) {
		t.Fatalf("long numbers: %v", err)
	}
	if _, err := s.Speak(context.Background(), "hello 42", ""); err != nil {
		t.Fatalf("ordinary text: %v", err)
	}
}

// A text with nothing speakable once rewritten answers 400 bad-text, not
// 503 not-ready.
func TestSpeakHandlerNothingSpeakableIsBadText(t *testing.T) {
	s, _ := testSpeaker(t)
	h := &SpeakHandler{Engine: s, Model: s.Model, Voice: "bf_emma", MaxChars: s.MaxChars}
	for _, text := range []string{"привет мир", "日本語", "😀 🎉"} {
		body, _ := json.Marshal(map[string]string{"text": text})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/speak", bytes.NewReader(body)))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), WordBadText) {
			t.Errorf("%q: %d %s", text, w.Code, w.Body)
		}
	}
}
