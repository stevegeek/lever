package termsafe

import "testing"

// TestSanitize pins the choke point for guest-supplied strings (scion
// stderr, hub slugs, container status, the record's image, error text)
// before they reach the operator's terminal: escape sequences are stripped
// whole, control bytes, format characters and blank glyphs are replaced, and
// ordinary UTF-8 passes unchanged.
func TestSanitize(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain ASCII unchanged", "manager is running (container running)", "manager is running (container running)"},
		{"plain UTF-8 unchanged", "✓ scaffold — current (lever-operator + workers) — café", "✓ scaffold — current (lever-operator + workers) — café"},
		{"empty", "", ""},
		{"OSC title (BEL-terminated) stripped whole", "before\x1b]0;doctor: all ok\x07after", "beforeafter"},
		{"OSC 52 clipboard (ST-terminated) stripped whole", "x\x1b]52;c;aGVsbG8=\x1b\\y", "xy"},
		{"CSI clear screen stripped", "\x1b[2Jrunning", "running"},
		{"CSI colour with params stripped", "\x1b[1;31mError\x1b[0m: nope", "Error: nope"},
		{"CSI cursor-up row spoof stripped", "ok\x1b[3A\x1b[2K✗ broker — dead", "ok✗ broker — dead"},
		{"two-byte ESC sequence stripped", "\x1bcreset", "reset"},
		{"nF charset sequence stripped", "\x1b(Btext", "text"},
		{"DCS stripped to ST", "a\x1bPq...\x1b\\b", "ab"},
		{"C1 CSI (U+009B) stripped", "a\u009b2Jb", "ab"},
		{"C1 OSC (U+009D) stripped to C1 ST", "a\u009d0;t\u009cb", "ab"},
		{"lone C1 byte replaced", "a\u0080b", "a�b"},
		{"C0 bytes replaced, tab and newline included", "a\tb\nc\rd\x00e", "a�b�c�d�e"},
		{"DEL replaced", "a\x7fb", "a�b"},
		{"bare trailing ESC dropped", "abc\x1b", "abc"},
		{"unterminated CSI dropped to end", "abc\x1b[1;2", "abc"},
		{"unterminated OSC dropped to end", "abc\x1b]0;title", "abc"},
		{"invalid UTF-8 byte replaced", "a\x9bb", "a�b"},
		{"bidi override and pop replaced", "ok \u202egnp.exe\u202c", "ok �gnp.exe�"},
		{"bidi embeddings replaced", "a\u202ab\u202bc\u202dd", "a�b�c�d"},
		{"bidi isolates replaced", "a\u2066b\u2067c\u2068d\u2069e", "a�b�c�d�e"},
		{"LRM, RLM and Arabic letter mark replaced", "a\u200eb\u200fc\u061cd", "a�b�c�d"},
		{"zero-width space, non-joiner and joiner replaced", "a\u200bb\u200cc\u200dd", "a�b�c�d"},
		{"BOM and word joiner replaced", "\ufeffa\u2060b", "�a�b"},
		{"soft hyphen replaced", "lev\u00adler", "lev�ler"},
		{"tag characters replaced", "a\U000e0001\U000e0041\U000e007fb", "a���b"},
		{"line and paragraph separators replaced", "a\u2028b\u2029c", "a�b�c"},
		{"blank glyphs replaced", "\u2800\u3164\u115f\u1160\uffa0[lever: x]", "�����[lever: x]"},
		{"combining marks kept", "e\u0301 n\u0303 \u0915\u093f \u05e9\u05c1 \u0627\u064e", "e\u0301 n\u0303 \u0915\u093f \u05e9\u05c1 \u0627\u064e"},
		{"variation selectors replaced", "\u2764\ufe0f a\ufe00b\U000e0100c\U000e01efd", "\u2764\ufffd a\ufffdb\ufffdc\ufffdd"},
		{"combining grapheme joiner replaced", "a\u034fb", "a\ufffdb"},
		{"Khmer inherent vowels replaced", "\u1780\u17b4\u17b5\u17b6", "\u1780\ufffd\ufffd\u17b6"},
		{"non-Latin scripts kept", "Ελληνικά 日本語 עברית العربية", "Ελληνικά 日本語 עברית العربية"},
	}
	for _, c := range cases {
		if got := Sanitize(c.in); got != c.want {
			t.Errorf("%s: Sanitize(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
