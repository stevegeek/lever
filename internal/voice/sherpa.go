package voice

// The sherpa-onnx adapter: every assumption lever makes about
// sherpa-onnx-offline-tts is in this file. Checked against sherpa-onnx's
// source on master (2026-10: sherpa-onnx-offline-tts.cc, parse-options.cc,
// the Kokoro and VITS model config and model files, wave-writer.cc) and its
// Kokoro documentation, not against a built release; tools/test/fakesherpa
// stands in for it in tests.
//
//  1. One run speaks one text: the program's one positional argument, the
//     WAV to --output-filename, exit 0 on success. It reads no stdin and
//     loads the model files on every run.
//  2. Options are Kaldi's --name=value. lever passes, for Kokoro,
//     --kokoro-model, --kokoro-voices, --kokoro-tokens, --kokoro-data-dir
//     (espeak-ng's data) and --kokoro-length-scale; for VITS (Piper)
//     --vits-model, --vits-tokens, --vits-data-dir and --vits-length-scale;
//     and --num-threads, --sid and --output-filename. Nothing else. The
//     length scale is the speed (larger is slower); --speed is not passed
//     because some releases that run Kokoro lack it. The data directory must
//     hold phontab, phonindex, phondata and intonations. kokoro-en-v0_19 is
//     a version 1 model: it takes no --kokoro-lexicon or --kokoro-lang (a
//     version 2 model would require one).
//  3. A first pass reads --config=FILE and --help from every argument
//     starting with "--" up to a lone "--", positional ones included; the
//     second reads options up to the first non-option or a lone "--", which
//     is dropped. lever passes the text last, after "--", so no text is ever
//     read as an option (not even --config), and never passes a text
//     starting with "-" (SpeechInput).
//  4. The output is a WAV with a 44-byte header, 16-bit PCM, mono, at the
//     model's sample rate. lever reads only its fmt and data chunks and
//     sends on a WAV it writes itself (SpeechWAV).
//  5. The program prints progress, the text, its arguments and timings to
//     stdout and stderr; lever discards both: text must never reach the
//     tool's log.
//  6. The text is plain text, not SSML. SpeechInput still removes control
//     and format characters and angle brackets.
//  7. A speaker id (--sid) out of range is not an error: the engine speaks
//     voice 0. lever's table must carry the documented ids
//     (speechmodels.go).

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The limits of read-aloud, for lever-tool-tts and the remote proxy.
const (
	// DefaultSpeakMaxChars is -max-chars when unset: the longest text of
	// one request. The page sends a message in parts of at most this many.
	DefaultSpeakMaxChars = 600
	MinSpeakMaxChars     = 100
	MaxSpeakMaxChars     = 2000
	// MinSpeed and MaxSpeed bound -speed (1 = the model's own pace).
	MinSpeed   = 0.5
	MaxSpeed   = 2.0
	MaxThreads = 16
	// maxSpeechSeconds bounds the audio of one request lever reads back:
	// even a slow voice speaks DefaultSpeakMaxChars in well under a minute.
	maxSpeechSeconds = 5 * 60
)

// SherpaArgs is the sherpa-onnx-offline-tts argument list (assumptions 1
// to 3): the options, "--", and the text, which must have passed
// SpeechInput.
func SherpaArgs(m SpeechModel, dir string, v SpeechVoice, speed float64, threads int, out, text string) []string {
	p := func(rel string) string { return SpeechPath(dir, m, rel) }
	scale := strconv.FormatFloat(1/speed, 'f', 3, 64)
	var args []string
	switch m.Kind {
	case KindKokoro:
		args = []string{"--kokoro-model=" + p(m.ModelFile), "--kokoro-voices=" + p(m.VoicesFile),
			"--kokoro-tokens=" + p(m.TokensFile), "--kokoro-data-dir=" + p(m.DataDir), "--kokoro-length-scale=" + scale}
	case KindVITS:
		args = []string{"--vits-model=" + p(m.ModelFile), "--vits-tokens=" + p(m.TokensFile),
			"--vits-data-dir=" + p(m.DataDir), "--vits-length-scale=" + scale}
	}
	return append(args, "--num-threads="+strconv.Itoa(threads), "--sid="+strconv.Itoa(v.SID),
		"--output-filename="+filepath.Clean(out), "--", text)
}

// ErrBadText: nothing speakable is left of the text, or it is too long.
var ErrBadText = errors.New("text")

// SpeechInput is text as lever hands it to the engine: valid UTF-8, control
// and format characters and angle brackets as spaces (no markup reaches the
// engine), whitespace runs collapsed, trimmed, and no leading "-"
// (assumption 3). It refuses an empty result or one over maxChars.
func SpeechInput(text string, maxChars int) (string, error) {
	text = strings.ToValidUTF8(text, " ")
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) || r == '<' || r == '>' || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	text = strings.TrimLeft(text, "-‐‑‒–—― ")
	switch n := utf8.RuneCountInString(text); {
	case n == 0 || !strings.ContainsFunc(text, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }):
		return "", fmt.Errorf("%w: nothing to speak", ErrBadText)
	case n > maxChars:
		return "", fmt.Errorf("%w: %d characters, at most %d", ErrBadText, n, maxChars)
	}
	return text, nil
}

// SpeechWAV reads the engine's WAV (assumption 4) and returns a canonical
// 44-byte-header WAV of its samples, at most maxSpeechSeconds long, so
// nothing else of the file goes on.
func SpeechWAV(b []byte, rate int) ([]byte, error) {
	le := binary.LittleEndian
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, errors.New("the engine's output is not a WAV file")
	}
	var fmtOK bool
	var data []byte
	for off := 12; off+8 <= len(b); {
		id, size := string(b[off:off+4]), int(le.Uint32(b[off+4:off+8]))
		body := b[off+8:]
		if size < 0 || size > len(body) {
			if id != "data" {
				return nil, errors.New("the engine's WAV has a chunk past its end")
			}
			// A writer that could not seek leaves the data size unset or
			// too large: take what is there.
			size = len(body)
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, errors.New("the engine's WAV has a short fmt chunk")
			}
			if le.Uint16(body[0:2]) != 1 || le.Uint16(body[2:4]) != 1 || int(le.Uint32(body[4:8])) != rate || le.Uint16(body[14:16]) != 16 {
				return nil, fmt.Errorf("the engine's WAV is not 16-bit PCM mono at %d Hz", rate)
			}
			fmtOK = true
		case "data":
			if !fmtOK {
				return nil, errors.New("the engine's WAV has data before its fmt chunk")
			}
			data = body[:size&^1]
		}
		if data != nil {
			break
		}
		off += 8 + size + size&1
	}
	switch {
	case data == nil:
		return nil, errors.New("the engine's WAV has no data chunk")
	case len(data) == 0:
		return nil, errors.New("the engine's WAV is empty")
	case len(data) > 2*rate*maxSpeechSeconds:
		return nil, errors.New("the engine's WAV is too long")
	}
	return pcmWAV(data, rate), nil
}

func pcmWAV(samples []byte, rate int) []byte {
	b := make([]byte, WAVHeaderLen+len(samples))
	le := binary.LittleEndian
	copy(b[0:], "RIFF")
	le.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	le.PutUint32(b[16:], 16)
	le.PutUint16(b[20:], 1)
	le.PutUint16(b[22:], 1)
	le.PutUint32(b[24:], uint32(rate))
	le.PutUint32(b[28:], uint32(2*rate))
	le.PutUint16(b[32:], 2)
	le.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	le.PutUint32(b[40:], uint32(len(samples)))
	copy(b[WAVHeaderLen:], samples)
	return b
}

func maxSpeechFile(rate int) int64 { return int64(2*rate*maxSpeechSeconds) + 4096 }
