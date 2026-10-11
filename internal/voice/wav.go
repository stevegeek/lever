package voice

import (
	"encoding/binary"
	"regexp"
	"strings"
	"unicode/utf8"
)

// The one audio format lever hands whisper-server: PCM s16le, 16 kHz, mono,
// in the canonical 44-byte WAV header (what the chat page encodes). The
// remote proxy checks a dictation clip with CheckWAV, and lever-tool-whisper
// checks it again on its socket, and checks an agent's file the same way.
const (
	SampleRate     = 16000
	BytesPerSecond = 2 * SampleRate // s16le mono
	WAVHeaderLen   = 44
	// MinClipBytes is a tenth of a second of samples: anything shorter holds
	// no word.
	MinClipBytes = BytesPerSecond / 10
	// MaxText is the chat message limit (scion's messages.MaxMessageLength,
	// chatcore.js MAX_MESSAGE), in characters: CleanText cuts to it.
	MaxText = 16000
)

// Refusal words of CheckWAV.
const (
	WordBadAudio = "bad-audio"
	WordTooLong  = "too-long"
)

// MaxClipBytes is the largest canonical clip of maxSeconds: the header and
// the samples.
func MaxClipBytes(maxSeconds int) int64 {
	return WAVHeaderLen + int64(BytesPerSecond)*int64(maxSeconds)
}

// CheckWAV checks a clip: the canonical 44-byte header of PCM s16le, 16 kHz,
// mono, every header field exact, the sizes matching the body, at least
// MinClipBytes of samples and at most maxSeconds. whisper.cpp then parses
// only a header lever has checked byte for byte. It answers the clip's
// length in seconds, or a refusal word (WordBadAudio, WordTooLong).
func CheckWAV(b []byte, maxSeconds int) (float64, string) {
	if len(b) < WAVHeaderLen+MinClipBytes {
		return 0, WordBadAudio
	}
	le := binary.LittleEndian
	data := len(b) - WAVHeaderLen
	ok := string(b[0:4]) == "RIFF" && le.Uint32(b[4:8]) == uint32(len(b)-8) && string(b[8:12]) == "WAVE" &&
		string(b[12:16]) == "fmt " && le.Uint32(b[16:20]) == 16 &&
		le.Uint16(b[20:22]) == 1 && // PCM
		le.Uint16(b[22:24]) == 1 && // mono
		le.Uint32(b[24:28]) == SampleRate &&
		le.Uint32(b[28:32]) == BytesPerSecond &&
		le.Uint16(b[32:34]) == 2 && // block align
		le.Uint16(b[34:36]) == 16 && // bits per sample
		string(b[36:40]) == "data" && le.Uint32(b[40:44]) == uint32(data) && data%2 == 0
	if !ok {
		return 0, WordBadAudio
	}
	if data > BytesPerSecond*maxSeconds {
		return 0, WordTooLong
	}
	return float64(data) / BytesPerSecond, ""
}

// CanonicalWAV is a silent canonical clip of n samples: the format CheckWAV
// accepts (tests in several packages build their clips with it).
func CanonicalWAV(n int) []byte {
	b := make([]byte, WAVHeaderLen+2*n)
	le := binary.LittleEndian
	copy(b[0:], "RIFF")
	le.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	le.PutUint32(b[16:], 16)
	le.PutUint16(b[20:], 1)
	le.PutUint16(b[22:], 1)
	le.PutUint32(b[24:], SampleRate)
	le.PutUint32(b[28:], BytesPerSecond)
	le.PutUint16(b[32:], 2)
	le.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	le.PutUint32(b[40:], uint32(2*n))
	return b
}

// CleanText is a transcript as a person or an agent gets it: valid UTF-8,
// Whisper's non-speech markers removed, trimmed, and cut to MaxText.
func CleanText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	// Whisper writes non-speech as markers ([BLANK_AUDIO], [MUSIC],
	// (silence)); they are not words anyone said. Only known ones are
	// removed, so dictated code such as a[i] or [TODO] stays.
	if whisperMarker.MatchString(s) {
		s = strings.TrimSpace(spaceRun.ReplaceAllString(whisperMarker.ReplaceAllString(s, " "), " "))
	}
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= MaxText {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:MaxText]))
}

// whisperMarker is one of Whisper's known non-speech markers, in square
// brackets or parentheses, with the spaces around it.
var whisperMarker = regexp.MustCompile(`(?i)[ \t]*[\[(]\s*(blank_audio|music|silence|noise|inaudible|applause|laughter|sound effect|sound-effect|no speech)\s*[\])][ \t]*`)

// spaceRun is a run of spaces or tabs (line breaks are kept).
var spaceRun = regexp.MustCompile(`[ \t]{2,}`)
