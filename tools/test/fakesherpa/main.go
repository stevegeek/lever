// Command fakesherpa is a TEST-ONLY stand-in for sherpa-onnx's
// sherpa-onnx-offline-tts: it takes the flags lever passes
// (internal/voice/sherpa.go) and writes a WAV to --output-filename, so a
// test can run lever-tool-tts without a model. Never part of a release
// (.goreleaser.yaml builds only binaries under ./cmd).
//
// The text steers it: one containing FAIL exits 2 with no output, SLEEP
// sleeps 30 s first, FLOAT writes a 32-bit float WAV, ARGV writes its
// argument list (NUL-separated) as the samples; any other text writes 100
// silent samples per byte, with a LIST chunk before the data.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	opts := map[string]string{}
	var pos []string
	for _, a := range os.Args[1:] {
		if k, v, ok := strings.Cut(strings.TrimPrefix(a, "--"), "="); ok && strings.HasPrefix(a, "--") {
			opts[k] = v
			continue
		}
		pos = append(pos, a)
	}
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "Error: Please provide the text to generate audio.")
		os.Exit(1)
	}
	text := pos[0]
	fmt.Println("text:", text) // as the real one prints it: lever must discard this
	model := opts["kokoro-model"]
	rate := 24000
	if model == "" {
		model, rate = opts["vits-model"], 22050
	}
	if _, err := os.Stat(model); err != nil {
		fmt.Fprintln(os.Stderr, "no model:", err)
		os.Exit(1)
	}
	out := opts["output-filename"]
	if out == "" {
		os.Exit(1)
	}
	switch {
	case strings.Contains(text, "FAIL"):
		os.Exit(2)
	case strings.Contains(text, "SLEEP"):
		time.Sleep(30 * time.Second)
	}
	var samples []byte
	format, bits := uint16(1), uint16(16)
	switch {
	case strings.Contains(text, "ARGV"):
		samples = []byte(strings.Join(os.Args[1:], "\x00"))
		if len(samples)%2 == 1 {
			samples = append(samples, 0)
		}
	case strings.Contains(text, "FLOAT"):
		format, bits = 3, 32
		samples = make([]byte, 400)
	default:
		samples = make([]byte, 200*len(text))
	}
	le := binary.LittleEndian
	var b []byte
	b = append(b, "RIFF\x00\x00\x00\x00WAVE"...)
	b = append(b, "fmt "...)
	b = le.AppendUint32(b, 16)
	b = le.AppendUint16(b, format)
	b = le.AppendUint16(b, 1)
	b = le.AppendUint32(b, uint32(rate))
	b = le.AppendUint32(b, uint32(rate)*uint32(bits/8))
	b = le.AppendUint16(b, bits/8)
	b = le.AppendUint16(b, bits)
	b = append(b, "LIST"...)
	b = le.AppendUint32(b, 3)
	b = append(b, "abc\x00"...) // odd size, padded
	b = append(b, "data"...)
	b = le.AppendUint32(b, uint32(len(samples)))
	b = append(b, samples...)
	le.PutUint32(b[4:], uint32(len(b)-8))
	if err := os.WriteFile(out, b, 0o600); err != nil {
		os.Exit(1)
	}
}
