package voice

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestCheckWAV(t *testing.T) {
	good := CanonicalWAV(16000)
	if sec, word := CheckWAV(good, 1); sec != 1 || word != "" {
		t.Fatalf("%v %q", sec, word)
	}
	if _, word := CheckWAV(CanonicalWAV(16001), 1); word != WordTooLong {
		t.Fatalf("over max: %q", word)
	}
	if int64(len(good)) != MaxClipBytes(1) {
		t.Fatalf("MaxClipBytes(1) = %d, a 1 s clip is %d", MaxClipBytes(1), len(good))
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
		"header only":  func(b []byte) []byte { return CanonicalWAV(0) },
		"truncated":    func(b []byte) []byte { return b[:30] },
		"empty":        func(b []byte) []byte { return nil },
		"short clip":   func(b []byte) []byte { return CanonicalWAV(799) },
		"riff in data": func(b []byte) []byte { return append([]byte("RIFF"), b...) },
	} {
		if _, word := CheckWAV(mut(bytes.Clone(good)), 1); word != WordBadAudio {
			t.Errorf("%s: %q", name, word)
		}
	}
	if _, word := CheckWAV(CanonicalWAV(1600), 1); word != "" {
		t.Fatalf("a tenth of a second: %q", word)
	}
}

func TestCleanTextDropsWhisperMarkers(t *testing.T) {
	for in, want := range map[string]string{
		" [BLANK_AUDIO]\n":                  "",
		"hello [MUSIC] world":               "hello world",
		"[ Silence ] ok":                    "ok",
		"(inaudible)":                       "",
		"keep a[i], [TODO] and (x) as said": "keep a[i], [TODO] and (x) as said",
		"line one\nline two":                "line one\nline two",
	} {
		if got := CleanText(in); got != want {
			t.Errorf("CleanText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanText(t *testing.T) {
	if got := CleanText(" \n hi there \t"); got != "hi there" {
		t.Fatalf("%q", got)
	}
	long := strings.Repeat("é", MaxText+10)
	if got := CleanText(long); len([]rune(got)) != MaxText {
		t.Fatalf("%d", len([]rune(got)))
	}
	if got := CleanText("a\xffb"); got != "a�b" {
		t.Fatalf("%q", got)
	}
}

func TestSettings(t *testing.T) {
	for _, ok := range []string{"", "en", "haw"} {
		if err := CheckLanguage(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"English", "auto", "EN", "e"} {
		if CheckLanguage(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if p, err := ParseVocabulary(" Lever, Scion ,mTLS"); err != nil || p != "Lever, Scion, mTLS" {
		t.Fatalf("%q %v", p, err)
	}
	if p, err := ParseVocabulary(""); err != nil || p != "" {
		t.Fatalf("%q %v", p, err)
	}
	for _, bad := range []string{"a,,b", "a\nb", strings.Repeat("w", 65), strings.Repeat(strings.Repeat("w", 60)+",", 20)} {
		if _, err := ParseVocabulary(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for n, ok := range map[int]bool{0: false, 1: true, 600: true, 601: false, -1: false} {
		if (CheckMaxSeconds(n) == nil) != ok {
			t.Errorf("max seconds %d", n)
		}
	}
}
