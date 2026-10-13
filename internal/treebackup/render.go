package treebackup

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const hexDigits = "0123456789abcdef"

// plainByte reports whether Q prints b as it is outside quotes.
func plainByte(b byte) bool {
	return b >= 0x21 && b <= 0x7e && b != '"' && b != '\\'
}

// Q renders tree bytes (a path, a link target, a name) for the terminal.
// It is injective: an unquoted result never contains '"' or '\\', a quoted
// one always starts with '"', and inside the quotes every byte has exactly
// one spelling. Every byte outside 0x20-0x7e is written as \xNN, so NFC and
// NFD forms, homoglyphs, bidi and zero-width characters all print
// differently.
func Q(b []byte) string {
	plain := len(b) > 0
	for _, c := range b {
		if !plainByte(c) {
			plain = false
			break
		}
	}
	if plain {
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(len(b) + 2)
	sb.WriteByte('"')
	for _, c := range b {
		switch {
		case c == '"':
			sb.WriteString(`\"`)
		case c == '\\':
			sb.WriteString(`\\`)
		case c == '\t':
			sb.WriteString(`\t`)
		case c == '\n':
			sb.WriteString(`\n`)
		case c == '\r':
			sb.WriteString(`\r`)
		case c >= 0x20 && c <= 0x7e:
			sb.WriteByte(c)
		default:
			sb.WriteString(`\x`)
			sb.WriteByte(hexDigits[c>>4])
			sb.WriteByte(hexDigits[c&0xf])
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// qs is Q for strings lever did not write itself but that are not tree
// paths, such as header fields the reader validated.
func qs(s string) string { return Q([]byte(s)) }

// ParseQ is the exact inverse of Q: it accepts only strings Q produces, so
// a path copied from lever's output names the same bytes and nothing else.
func ParseQ(s string) ([]byte, error) {
	if !strings.HasPrefix(s, `"`) {
		if s == "" {
			return nil, errors.New("empty path")
		}
		for i := 0; i < len(s); i++ {
			if !plainByte(s[i]) {
				return nil, fmt.Errorf("unquoted path has byte 0x%02x at offset %d: quote it the way lever prints it", s[i], i)
			}
		}
		return []byte(s), nil
	}
	if len(s) < 2 || s[len(s)-1] != '"' {
		return nil, errors.New("quoted path has no closing quote")
	}
	in := s[1 : len(s)-1]
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c == '"' {
			return nil, fmt.Errorf("unescaped quote at offset %d", i+1)
		}
		if c != '\\' {
			out = append(out, c)
			continue
		}
		if i+1 >= len(in) {
			return nil, errors.New("quoted path ends in a lone backslash")
		}
		i++
		switch in[i] {
		case '"', '\\':
			out = append(out, in[i])
		case 't':
			out = append(out, '\t')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 'x':
			if i+2 >= len(in) {
				return nil, errors.New(`truncated \x escape`)
			}
			hi, lo := unhex(in[i+1]), unhex(in[i+2])
			if hi < 0 || lo < 0 {
				return nil, fmt.Errorf(`bad \x escape at offset %d (two lowercase hex digits)`, i)
			}
			out = append(out, byte(hi<<4|lo))
			i += 2
		default:
			return nil, fmt.Errorf("unknown escape \\%c at offset %d", in[i], i)
		}
	}
	// Anything Q would have spelled differently (an escaped printable byte,
	// a raw control byte, quotes around a plain path) is not Q's output.
	if Q(out) != s {
		return nil, fmt.Errorf("not in the form lever prints: expected %s", Q(out))
	}
	return out, nil
}

func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return -1
}

// QPrefix marks a path argument in Q form.
const QPrefix = "q:"

// ParsePathArg turns a command-line path into tree bytes. "q:" followed by
// Q's form is parsed exactly; anything else is taken as raw argv bytes, so
// a path that itself starts with "q:" must be given in the q: form. Nothing
// is cleaned or stripped: a trailing slash, "./x" or "a//b" is an error, not
// a different path.
func ParsePathArg(arg string) ([]byte, error) {
	var p []byte
	if rest, ok := strings.CutPrefix(arg, QPrefix); ok {
		var err error
		if p, err = ParseQ(rest); err != nil {
			return nil, fmt.Errorf("path %s: %w", qs(arg), err)
		}
	} else {
		p = []byte(arg)
	}
	if err := checkRelPath(p); err != nil {
		return nil, fmt.Errorf("path %s: %w", Q(p), err)
	}
	return p, nil
}

func checkRelPath(p []byte) error {
	switch {
	case len(p) == 0:
		return errors.New("empty")
	case len(p) > MaxPathCap:
		return fmt.Errorf("longer than %d bytes", MaxPathCap)
	case p[0] == '/':
		return errors.New("absolute: paths are relative to the tree")
	case bytes.IndexByte(p, 0) >= 0:
		return errors.New("contains a NUL byte")
	}
	for c := range bytes.SplitSeq(p, []byte{'/'}) {
		switch string(c) {
		case "":
			return errors.New("has an empty component")
		case ".", "..":
			return fmt.Errorf("has a %q component", c)
		}
	}
	return nil
}

// Content lines of a text diff.

// maxRawLine is the longest line printed; longer lines are cut, and always
// escaped so the cut marker cannot be forged by content.
const maxRawLine = 1000

// rawSafe reports whether r may reach the terminal as it is. Visible means
// in one of the L, M, N, P, S or Zs tables of the unicode package. Code
// points in no table are unassigned (as of the package's Unicode version);
// together with Cc, Cf, Co, Cs, Zl and Zp they are escaped. Variation
// selectors and tag characters are M or Cf but are named explicitly because
// they change how neighbouring text looks.
func rawSafe(r rune) bool {
	switch {
	case r == '\t':
		return true
	case r < 0x20 || r == 0x7f:
		return false
	case r < 0x7f:
		return true
	case r >= 0xfe00 && r <= 0xfe0f, r >= 0xe0100 && r <= 0xe01ef, r >= 0xe0000 && r <= 0xe007f:
		return false
	case r == 0x2028 || r == 0x2029:
		return false
	case unicode.Is(unicode.Cf, r):
		return false
	}
	return unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Zs)
}

// contentLine renders one line of file content (without its newline) after
// a two-byte prefix: op (' ', '+', '-'), then ' ' for a raw line or '\\'
// for an escaped one. In an escaped line '\\' is doubled and every unsafe
// character is \xNN (a byte under 0x80, or a byte that is not UTF-8) or
// \u{XXXX}; a cut line ends in \u{2026}(+N bytes), which no content can
// produce because U+2026 itself is printed raw.
func contentLine(op byte, line []byte) string {
	raw := len(line) <= maxRawLine && utf8.Valid(line)
	if raw {
		for _, r := range string(line) {
			if !rawSafe(r) {
				raw = false
				break
			}
		}
	}
	var sb strings.Builder
	sb.WriteByte(op)
	if raw {
		sb.WriteByte(' ')
		sb.Write(line)
		return sb.String()
	}
	sb.WriteByte('\\')
	body := line
	cut := 0
	if len(line) > maxRawLine {
		n := maxRawLine
		for n > 0 && n < len(line) && !utf8.RuneStart(line[n]) {
			n--
		}
		body, cut = line[:n], len(line)-n
	}
	for i := 0; i < len(body); {
		r, size := utf8.DecodeRune(body[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			writeHexByte(&sb, body[i])
			size = 1
		case r == '\\':
			sb.WriteString(`\\`)
		case !rawSafe(r) && r < 0x80:
			writeHexByte(&sb, byte(r))
		case !rawSafe(r):
			fmt.Fprintf(&sb, `\u{%04X}`, r)
		default:
			sb.Write(body[i : i+size])
		}
		i += size
	}
	if cut > 0 {
		fmt.Fprintf(&sb, `\u{2026}(+%d bytes)`, cut)
	}
	return sb.String()
}

func writeHexByte(sb *strings.Builder, c byte) {
	sb.WriteString(`\x`)
	sb.WriteByte(hexDigits[c>>4])
	sb.WriteByte(hexDigits[c&0xf])
}

// safeText renders a lever-built message (an error, which already quotes
// tree paths with Q) as it is when every character may reach the terminal
// raw, and with Q otherwise.
func safeText(s string) string {
	if !utf8.ValidString(s) {
		return qs(s)
	}
	for _, r := range s {
		if r == '\t' || !rawSafe(r) {
			return qs(s)
		}
	}
	return s
}
