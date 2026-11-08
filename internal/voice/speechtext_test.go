package voice

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testLexicon() Lexicon {
	lex := Lexicon{}
	for _, w := range strings.Fields("hello there the agents opened on git hub and returned config i updated tests pass " +
		"don't exception zero one two three four five six seven eight nine ten eleven twelve thirteen twenty forty " +
		"fifty hundred thousand million billion point percent plus at first second third fourth") {
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
	} {
		if got := SpeakableText(in, lex); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
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
