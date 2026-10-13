// Package termsafe makes guest-supplied strings safe to print on the
// operator's terminal. It is a stdlib-only leaf so every host-side surface
// that relays text from the jail can share this one choke point.
package termsafe

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Sanitize makes a string a compromised jail chose safe to print raw on the
// operator's terminal, where it could otherwise write an OSC title or the
// clipboard, overdraw earlier rows with a fake verdict, or use a C1 byte a
// Latin-1 terminal reads as an introducer. Every ESC/C1-introduced sequence
// is stripped whole. Other C0 bytes, DEL, lone C1 code points, invalid
// UTF-8, format characters (Cf: bidi overrides, zero-width, tags), line and
// paragraph separators, blank glyphs and marks that draw nothing become
// U+FFFD: reordered or hidden text could make a jail string read as lever's
// own. Go's %q does not escape the blank glyphs, which are printable.
func Sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			b.WriteRune(utf8.RuneError)
		case r == 0x1b: // ESC: skip the sequence it introduces
			n += escapeTail(s[i+1:])
		case r >= 0x80 && r <= 0x9f: // C1
			if tail, ok := c1Sequence(r, s[i+n:]); ok {
				n += tail
			} else {
				b.WriteRune(utf8.RuneError)
			}
		case r < 0x20 || r == 0x7f: // C0, DEL
			b.WriteRune(utf8.RuneError)
		case invisible(r):
			b.WriteRune(utf8.RuneError)
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// invisible: the braille blank and Hangul fillers render as blank; the
// variation selectors, U+034F and the Khmer inherent vowels draw nothing.
// Other combining marks pass because scripts need them.
func invisible(r rune) bool {
	switch r {
	case 0x2028, 0x2029, 0x2800, 0x3164, 0x115f, 0x1160, 0xffa0,
		0x034f, 0x17b4, 0x17b5:
		return true
	}
	if (r >= 0xfe00 && r <= 0xfe0f) || (r >= 0xe0100 && r <= 0xe01ef) {
		return true
	}
	return unicode.Is(unicode.Cf, r)
}

// escapeTail returns how many bytes of s (the text after an ESC) belong to
// the sequence the ESC introduces. Unterminated sequences run to the end of
// s, as a terminal would swallow them.
func escapeTail(s string) int {
	if s == "" {
		return 0
	}
	switch s[0] {
	case '[': // CSI: params 0x30-0x3F, intermediates 0x20-0x2F, final 0x40-0x7E
		return 1 + csiTail(s[1:])
	case ']', 'P', 'X', '^', '_': // OSC, DCS, SOS, PM, APC: to BEL or ST
		return 1 + stringTail(s[1:])
	}
	// nF (ESC + intermediates 0x20-0x2F + final 0x30-0x7E) and the two-byte
	// Fp/Fe/Fs escapes (ESC + 0x30-0x7E).
	i := 0
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
		i++
	}
	if i < len(s) && s[i] >= 0x30 && s[i] <= 0x7e {
		return i + 1
	}
	// A control byte or the end: the ESC is dropped on its own; what follows
	// is handled by the main loop.
	return i
}

// c1Sequence handles the single-code-point CSI, OSC, DCS, SOS, PM and APC;
// ok is false for any other C1.
func c1Sequence(r rune, rest string) (int, bool) {
	switch r {
	case 0x9b:
		return csiTail(rest), true
	case 0x9d, 0x90, 0x98, 0x9e, 0x9f:
		return stringTail(rest), true
	}
	return 0, false
}

func csiTail(s string) int {
	i := 0
	for i < len(s) && s[i] >= 0x30 && s[i] <= 0x3f {
		i++
	}
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
		i++
	}
	if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
		return i + 1
	}
	return i
}

// stringTail consumes up to and including a BEL, an ESC-backslash ST, or a
// C1 ST (U+009C); an unterminated string runs to the end.
func stringTail(s string) int {
	for i := 0; i < len(s); {
		switch {
		case s[i] == 0x07:
			return i + 1
		case s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\':
			return i + 2
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == 0x9c {
			return i + n
		}
		i += n
	}
	return len(s)
}
