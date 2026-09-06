package termsafe

import "testing"

// TestSanitize pins the choke point for guest-supplied strings (scion
// stderr, hub slugs, container status, the record's image, error text)
// before they reach the operator's terminal: escape sequences are stripped
// whole, control bytes are replaced, and ordinary UTF-8 passes unchanged.
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
	}
	for _, c := range cases {
		if got := Sanitize(c.in); got != c.want {
			t.Errorf("%s: Sanitize(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
