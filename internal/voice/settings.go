package voice

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// The limits of one clip, for dictation (remote.voice.max_seconds) and for
// lever-tool-whisper (-max-seconds) alike.
const (
	DefaultMaxSeconds = 300
	MaxMaxSeconds     = 600
	// MaxVocabulary bounds the vocabulary as a prompt, in bytes (Whisper
	// reads a short prompt; the rest would be cut anyway).
	MaxVocabulary = 800
	// maxWord bounds one vocabulary entry, in bytes.
	maxWord = 64
)

// languageRE is a Whisper language code: two or three lowercase letters
// ("en", "de", "haw").
var languageRE = regexp.MustCompile(`^[a-z]{2,3}$`)

// CheckLanguage accepts "" (detect the language per clip) or a Whisper
// language code.
func CheckLanguage(lang string) error {
	if lang != "" && !languageRE.MatchString(lang) {
		return fmt.Errorf("language %q: a Whisper language code such as en or de (leave it out to detect the language)", lang)
	}
	return nil
}

// ParseVocabulary turns a comma list of words Whisper should expect (names,
// jargon) into its prompt: the words, trimmed, joined with ", ". Each entry
// is one word or short phrase of 1 to 64 bytes with no control character,
// and the prompt stays within MaxVocabulary bytes. "" is no prompt.
func ParseVocabulary(list string) (string, error) {
	if strings.TrimSpace(list) == "" {
		return "", nil
	}
	var words []string
	for _, w := range strings.Split(list, ",") {
		w = strings.TrimSpace(w)
		if w == "" || len(w) > maxWord || strings.ContainsFunc(w, unicode.IsControl) {
			return "", fmt.Errorf("vocabulary entry %q: one word or short phrase, 1 to %d bytes, no control characters", w, maxWord)
		}
		words = append(words, w)
	}
	prompt := strings.Join(words, ", ")
	if len(prompt) > MaxVocabulary {
		return "", fmt.Errorf("vocabulary is %d bytes as a prompt; keep it under %d (Whisper reads only a short prompt)", len(prompt), MaxVocabulary)
	}
	return prompt, nil
}

// CheckMaxSeconds accepts 1 to MaxMaxSeconds.
func CheckMaxSeconds(n int) error {
	if n < 1 || n > MaxMaxSeconds {
		return fmt.Errorf("max seconds %d; use 1 to %d", n, MaxMaxSeconds)
	}
	return nil
}
