package voice

// The sherpa-onnx adapter: every assumption lever makes about sherpa-onnx's
// text-to-speech program, sherpa-onnx-offline-tts, is in this file. Check
// them against the sherpa-onnx version in use:
//
//  1. One run speaks one text: the text is the program's one positional
//     argument, the WAV goes to --output-filename, and the program exits
//     (status 0 on success). It reads no stdin and opens no network
//     connection; it loads the model files it is given on every run.
//  2. Flags, in Kaldi's --name=value form: for Kokoro --kokoro-model,
//     --kokoro-voices, --kokoro-tokens, --kokoro-data-dir (espeak-ng's
//     data) and --kokoro-length-scale (speed: larger is slower); for a
//     VITS (Piper) model --vits-model, --vits-tokens, --vits-data-dir and
//     --vits-length-scale; and --num-threads, --sid (the speaker id) and
//     --output-filename. Nothing else is passed.
//  3. An argument that starts with "--" is read as an option, wherever it
//     is. lever never passes a text that starts with "-" (SpeechInput).
//  4. The output is a WAV file of 16-bit PCM, mono, at the model's sample
//     rate. lever reads only the fmt and data chunks of it and sends on a
//     WAV it writes itself (SpeechWAV): nothing else of the file leaves the
//     tool.
//  5. The program prints the text it speaks, and timings, to its console.
//     lever discards the child's stdout and stderr: text must never reach
//     the tool's log.
//  6. The text is not SSML or any other markup: sherpa-onnx speaks it as
//     plain text (through espeak-ng's phonemizer for Kokoro and Piper
//     models). SpeechInput still removes control and format characters and
//     angle brackets.
//
// Checked against sherpa-onnx's documentation of its Kokoro and Piper
// models and the offline TTS program (sherpa-onnx/csrc/
// sherpa-onnx-offline-tts.cc and offline-tts-*-model-config.cc), as lever's
// authors knew them; NOT re-checked against a release when this was
// written. The fake in tools/test/fakesherpa stands in for it in tests.

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
	// DefaultSpeakMaxChars is lever-tool-tts's -max-chars when unset: the
	// longest text of one request, in characters. The page sends a message
	// in parts of at most this many, sentence by sentence.
	DefaultSpeakMaxChars = 600
	MinSpeakMaxChars     = 100
	MaxSpeakMaxChars     = 2000
	// MinSpeed and MaxSpeed bound -speed (1 = the model's own pace).
	MinSpeed = 0.5
	MaxSpeed = 2.0
	// MaxThreads bounds -threads.
	MaxThreads = 16
	// maxSpeechSeconds bounds the audio of one request lever reads back:
	// even a slow voice speaks DefaultSpeakMaxChars characters in well
	// under a minute.
	maxSpeechSeconds = 5 * 60
)

// SherpaArgs is the sherpa-onnx-offline-tts argument list that speaks text
// with voice v of model m from the models directory dir into out
// (assumptions 1 to 3). text must have passed SpeechInput.
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
		"--output-filename="+filepath.Clean(out), text)
}

// ErrBadText: nothing speakable is left of the text, or it is too long.
var ErrBadText = errors.New("text")

// SpeechInput is text as lever hands it to the engine: valid UTF-8; control
// and format characters, and angle brackets, as spaces (no markup reaches
// the engine); runs of white space as one space; trimmed; and no leading
// "-" (assumption 3). It refuses an empty result, and one longer than
// maxChars characters (the caller splits a long text).
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

// SpeechWAV reads the engine's WAV (assumption 4): RIFF/WAVE, a fmt chunk
// of 16-bit PCM, mono, at rate, and a data chunk; other chunks are
// skipped. It returns a canonical 44-byte-header WAV of those samples, at
// most maxSpeechSeconds long, so nothing else of the file goes on.
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

// pcmWAV is samples (16-bit PCM, mono) at rate in the canonical 44-byte
// header.
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

// maxSpeechFile bounds the engine's output file lever reads.
func maxSpeechFile(rate int) int64 { return int64(2*rate*maxSpeechSeconds) + 4096 }
