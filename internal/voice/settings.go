package voice

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// The limits of one clip, for dictation and lever-tool-whisper alike.
const (
	DefaultMaxSeconds = 300
	MaxMaxSeconds     = 600
	// DefaultAgentMaxSeconds is -agent-max-seconds when unset: it bounds how
	// long one agent clip can hold the GPU ahead of a waiting dictation clip.
	DefaultAgentMaxSeconds = 120
	// MaxVocabulary bounds the vocabulary prompt in bytes; Whisper reads a
	// short prompt and would cut the rest anyway.
	MaxVocabulary = 800
	maxWord       = 64
)

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
// jargon) into its prompt. Each entry is 1 to 64 bytes with no control
// character, and the prompt stays within MaxVocabulary. "" is no prompt.
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

func CheckMaxSeconds(n int) error {
	if n < 1 || n > MaxMaxSeconds {
		return fmt.Errorf("max seconds %d; use 1 to %d", n, MaxMaxSeconds)
	}
	return nil
}

// CheckAgentMaxSeconds accepts 1 to maxSeconds (the tool's -max-seconds).
func CheckAgentMaxSeconds(n, maxSeconds int) error {
	if n < 1 || n > maxSeconds {
		return fmt.Errorf("agent max seconds %d; use 1 to %d (the tool's -max-seconds)", n, maxSeconds)
	}
	return nil
}

// AgentMaxSeconds is n when set, else DefaultAgentMaxSeconds, never more than
// maxSeconds.
func AgentMaxSeconds(n, maxSeconds int) int {
	if n <= 0 {
		n = DefaultAgentMaxSeconds
	}
	return min(n, maxSeconds)
}

func CheckSpeed(s float64) error {
	if !(s >= MinSpeed && s <= MaxSpeed) {
		return fmt.Errorf("speed %v; use %.1f to %.1f (1 = the model's own pace)", s, MinSpeed, MaxSpeed)
	}
	return nil
}

func CheckThreads(n int) error {
	if n < 1 || n > MaxThreads {
		return fmt.Errorf("threads %d; use 1 to %d", n, MaxThreads)
	}
	return nil
}

func CheckSpeakMaxChars(n int) error {
	if n < MinSpeakMaxChars || n > MaxSpeakMaxChars {
		return fmt.Errorf("max chars %d; use %d to %d", n, MinSpeakMaxChars, MaxSpeakMaxChars)
	}
	return nil
}
